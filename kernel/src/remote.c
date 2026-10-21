/* remote.c —— 通过 SSH 在服务器上执行工具，见 remote.h */
#include "remote.h"
#include "util.h"

#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/utsname.h>
#include <sys/wait.h>
#include <unistd.h>

#define RETRY_MS     60000    /* 准备失败后多久再试 */
#define RECHECK_MS   300000   /* 准备好的服务器多久再检查一次 */
#define MAX_UPLOAD   (64u << 20)

static const char *state_name(rm_state s)
{
    switch (s) {
    case RM_IDLE:   return "等待检查";
    case RM_PROBE:  return "正在检查执行器";
    case RM_UPLOAD: return "正在上传执行器";
    case RM_BUILD:  return "正在服务器上编译执行器";
    case RM_READY:  return "就绪";
    case RM_FAILED: return "不可用";
    }
    return "";
}

/* ---- 小工具 ---- */

/* 单引号包起来，给 shell 用（sh、bash、zsh、fish 都认） */
char *shell_quote(const char *s)
{
    buf_t b = {0};
    buf_puts(&b, "'");
    for (const char *p = s; *p; p++) {
        if (*p == '\'') buf_puts(&b, "'\\''");
        else buf_append(&b, p, 1);
    }
    buf_puts(&b, "'");
    return buf_cstr(&b);
}

void argv_free(char **argv)
{
    if (!argv) return;
    for (char **p = argv; *p; p++) free(*p);
    free(argv);
}

static char *read_file(const char *path, size_t *len)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return NULL;
    buf_t b = {0};
    char tmp[65536];
    ssize_t n;
    while ((n = read(fd, tmp, sizeof tmp)) > 0) {
        buf_append(&b, tmp, (size_t)n);
        if (b.len > MAX_UPLOAD) break;
    }
    close(fd);
    if (n < 0 || b.len > MAX_UPLOAD) {
        buf_free(&b);
        return NULL;
    }
    *len = b.len;
    if (!b.p) b.p = xstrdup("");
    return b.p;
}

/* FNV-1a 64 位：只用来区分执行器的版本，不需要抗碰撞 */
static char *fingerprint(const char *path)
{
    size_t n = 0;
    char *data = read_file(path, &n);
    if (!data) return NULL;
    uint64_t h = 1469598103934665603ULL;
    for (size_t i = 0; i < n; i++) {
        h ^= (unsigned char)data[i];
        h *= 1099511628211ULL;
    }
    free(data);
    return xasprintf("%016llx", (unsigned long long)h);
}

static char *dir_of(const char *path)
{
    const char *slash = strrchr(path, '/');
    if (!slash) return xstrdup(".");
    return xasprintf("%.*s", (int)(slash - path), path);
}

static bool exists(const char *path)
{
    struct stat st;
    return stat(path, &st) == 0 && S_ISREG(st.st_mode);
}

/* ssh ControlPath 所在的目录：运行时目录下只有自己能进的 nano-ssh */
static char *control_dir(void)
{
    const char *run = getenv("XDG_RUNTIME_DIR");
    char *base = run && run[0] == '/' ? xstrdup(run) : xasprintf("/tmp/nano-agent-%d", (int)getuid());
    mkdir(base, 0700);
    char *dir = xasprintf("%s/nano-ssh", base);
    free(base);
    mkdir(dir, 0700);
    struct stat st;
    if (lstat(dir, &st) < 0 || !S_ISDIR(st.st_mode) || st.st_uid != getuid() || (st.st_mode & 077)) {
        free(dir);
        return NULL;
    }
    return dir;
}

/* ---- 初始化 ---- */

int remotes_init(remotes_t *rs, const kconf *conf, char *err, size_t errlen)
{
    memset(rs, 0, sizeof *rs);
    rs->conf = conf;
    if (!conf->nremotes) return 0;
    rs->list = xcalloc(conf->nremotes, sizeof *rs->list);
    rs->n = conf->nremotes;
    for (size_t i = 0; i < rs->n; i++) rs->list[i].conf = &conf->remotes[i];

    struct utsname u;
    uname(&u);
    snprintf(rs->arch, sizeof rs->arch, "%s", u.machine);
    rs->ctl_dir = control_dir();
    if (!rs->ctl_dir) {
        snprintf(err, errlen, "没法创建 ssh 连接复用用的目录（$XDG_RUNTIME_DIR/nano-ssh 或 /tmp/nano-agent-<uid>/nano-ssh）");
        return -1;
    }
    char *dir = dir_of(conf->exec);
    char *st = xasprintf("%s/nano-exec-static", dir);
    rs->upload_bin = exists(st) ? st : (free(st), xstrdup(conf->exec));
    char *tar = xasprintf("%s/nano-exec-src.tar.gz", dir);
    if (exists(tar)) rs->src_tar = tar;
    else free(tar);
    free(dir);
    rs->ver = fingerprint(rs->upload_bin);
    if (!rs->ver) {
        snprintf(err, errlen, "读不了执行器 %s：%s", rs->upload_bin, strerror(errno));
        return -1;
    }
    return 0;
}

void remotes_free(remotes_t *rs)
{
    for (size_t i = 0; i < rs->n; i++) {
        free(rs->list[i].detail);
        free(rs->list[i].arch);
        free(rs->list[i].cc);
    }
    free(rs->list);
    free(rs->ver);
    free(rs->ctl_dir);
    free(rs->upload_bin);
    free(rs->src_tar);
    memset(rs, 0, sizeof *rs);
}

remote_t *remotes_find(remotes_t *rs, const char *name)
{
    for (size_t i = 0; name && i < rs->n; i++)
        if (!strcmp(rs->list[i].conf->name, name)) return &rs->list[i];
    return NULL;
}

int remotes_index(const remotes_t *rs, const remote_t *rm)
{
    return (int)(rm - rs->list);
}

/* ---- ssh 命令 ---- */

/* ssh … 目标 sh -c '脚本' */
static char **ssh_argv(const remotes_t *rs, const remote_t *rm, const char *script)
{
    char *q = shell_quote(script);
    char *items[] = {
        xstrdup(rs->conf->ssh), xstrdup("-T"),
        xstrdup("-o"), xstrdup("BatchMode=yes"),
        xstrdup("-o"), xstrdup("ConnectTimeout=10"),
        xstrdup("-o"), xstrdup("ServerAliveInterval=15"),
        xstrdup("-o"), xstrdup("ServerAliveCountMax=3"),
        xstrdup("-o"), xstrdup("ControlMaster=auto"),
        xstrdup("-o"), xasprintf("ControlPath=%s/%%C", rs->ctl_dir),
        xstrdup("-o"), xstrdup("ControlPersist=600"),
        xstrdup("--"), xstrdup(rm->conf->target),
        xasprintf("sh -c %s", q),
    };
    free(q);
    size_t n = sizeof items / sizeof *items;
    char **argv = xcalloc(n + 1, sizeof *argv);
    memcpy(argv, items, sizeof items);
    return argv;
}

/* 服务器上执行器的路径（shell 表达式） */
static char *exe_expr(const remotes_t *rs, const remote_t *rm)
{
    if (rm->conf->exec) return shell_quote(rm->conf->exec);
    return xasprintf("\"$HOME/.cache/nano-agent/nano-exec-%s\"", rs->ver);
}

char **remote_exec_argv(const remotes_t *rs, const remote_t *rm, const char *args)
{
    char *exe = exe_expr(rs, rm);
    char *script = xasprintf("exec %s%s%s", exe, args ? " " : "", args ? args : "");
    char **argv = ssh_argv(rs, rm, script);
    free(script);
    free(exe);
    return argv;
}

/* ---- 准备服务器 ---- */

/* 网关只关心能不能用：就绪 ⇄ 不可用、或者不可用的原因变了才通知；中间的准备阶段不算 */
static void set_state(remotes_t *rs, remote_t *rm, rm_state st, const char *detail)
{
    bool same = (!detail && !rm->detail) || (detail && rm->detail && !strcmp(detail, rm->detail));
    if ((rm->state == RM_READY) != (st == RM_READY) || (st == RM_FAILED && (rm->state != RM_FAILED || !same)))
        rs->changed = true;
    rm->state = st;
    free(rm->detail);
    rm->detail = detail ? xstrdup(detail) : NULL;
}

static void start_prep(remotes_t *rs, remote_t *rm, runner_t *r, rm_state st, const char *script,
                       char *input, size_t in_len, int64_t now)
{
    char **argv = ssh_argv(rs, rm, script);
    char err[256];
    char id[96];
    snprintf(id, sizeof id, "prep:%s", rm->conf->name);
    job_t *j = runner_start_ex(r, argv, "", id, "prep", input ? input : xstrdup(""), input ? in_len : 0, err, sizeof err);
    argv_free(argv);
    if (!j) {
        remote_failed(rs, rm, err, now);
        return;
    }
    j->kind = JOB_PREP;
    j->tag = remotes_index(rs, rm);
    j->close_in = true;
    rm->busy = true;
    if (rm->state == RM_READY && st == RM_PROBE) return;   /* 定期检查：结果出来之前照常可用 */
    set_state(rs, rm, st, NULL);
    log_info("服务器 %s：%s", rm->conf->name, state_name(st));
}

static void start_probe(remotes_t *rs, remote_t *rm, runner_t *r, int64_t now)
{
    char *exe = exe_expr(rs, rm);
    char *script = xasprintf(
        "f=%s; if [ -x \"$f\" ] && o=$(\"$f\" --probe 2>/dev/null); then echo \"OK $o\"; "
        "else echo \"NEED $(uname -m) $(command -v cc || command -v gcc || true)\"; fi", exe);
    start_prep(rs, rm, r, RM_PROBE, script, NULL, 0, now);
    free(script);
    free(exe);
}

static void start_upload(remotes_t *rs, remote_t *rm, runner_t *r, int64_t now)
{
    size_t n = 0;
    char *bin = read_file(rs->upload_bin, &n);
    if (!bin) {
        remote_failed(rs, rm, "读不了本机的执行器，没法上传", now);
        return;
    }
    char *script = xasprintf(
        "d=\"$HOME/.cache/nano-agent\"; f=\"$d/nano-exec-%s\"; "
        "mkdir -p \"$d\" && chmod 700 \"$d\" && cat > \"$f.tmp\" && chmod 700 \"$f.tmp\" && mv -f \"$f.tmp\" \"$f\" || exit 1; "
        "if o=$(\"$f\" --probe 2>&1); then echo \"OK $o\"; else rm -f \"$f\"; echo \"FAIL $o\"; fi", rs->ver);
    start_prep(rs, rm, r, RM_UPLOAD, script, bin, n, now);
    free(script);
}

static void start_build(remotes_t *rs, remote_t *rm, runner_t *r, int64_t now)
{
    size_t n = 0;
    char *tar = read_file(rs->src_tar, &n);
    if (!tar) {
        remote_failed(rs, rm, "读不了执行器的源码包，没法在服务器上编译", now);
        return;
    }
    char *cc = shell_quote(rm->cc);
    char *script = xasprintf(
        "d=\"$HOME/.cache/nano-agent\"; s=\"$d/src-%s\"; f=\"$d/nano-exec-%s\"; "
        "mkdir -p \"$s\" && chmod 700 \"$d\" && cd \"$s\" && tar xzf - || exit 1; "
        "if %s -std=c11 -O2 -D_DEFAULT_SOURCE -D_POSIX_C_SOURCE=200809L -Isrc -Ithird_party/cjson -o \"$f.tmp\" "
        "src/nexec_main.c src/nexec_fs.c src/nexec_proc.c src/sandbox.c src/util.c third_party/cjson/cJSON.c -lm > build.log 2>&1 "
        "&& mv -f \"$f.tmp\" \"$f\"; then "
        "if o=$(\"$f\" --probe 2>&1); then echo \"OK $o\"; else echo \"FAIL $o\"; fi; "
        "else echo \"FAIL $(tail -n 3 build.log)\"; fi", rs->ver, rs->ver, cc);
    free(cc);
    start_prep(rs, rm, r, RM_BUILD, script, tar, n, now);
    free(script);
}

void remote_failed(remotes_t *rs, remote_t *rm, const char *why, int64_t now)
{
    if (rm->state != RM_FAILED || !rm->detail || strcmp(rm->detail, why))
        log_warn("服务器 %s 不可用：%s", rm->conf->name, why);
    set_state(rs, rm, RM_FAILED, why);
    rm->next_at = now + RETRY_MS;
    rm->busy = false;
}

void remotes_tick(remotes_t *rs, runner_t *r, int64_t now)
{
    for (size_t i = 0; i < rs->n; i++) {
        remote_t *rm = &rs->list[i];
        bool due = !rm->busy && (rm->state == RM_IDLE ||
                   ((rm->state == RM_FAILED || rm->state == RM_READY) && now >= rm->next_at));
        if (due) start_probe(rs, rm, r, now);
    }
}

/* 输出的最后一行（去掉结尾的换行），ssh 有时会在前面打印别的东西 */
static char *last_line(const buf_t *b)
{
    size_t n = b->len;
    while (n && (b->p[n - 1] == '\n' || b->p[n - 1] == '\r')) n--;
    size_t s = n;
    while (s && b->p[s - 1] != '\n') s--;
    return xasprintf("%.*s", (int)(n - s), b->p ? b->p + s : "");
}

/* stderr 里第一行有用的话（ssh 的错误信息） */
static char *first_err(const job_t *j)
{
    if (!j->err.len) return xstrdup(WIFEXITED(j->status) ? "（没有输出）" : "被终止了");
    size_t n = 0;
    while (n < j->err.len && j->err.p[n] != '\n') n++;
    buf_t b = {0};
    buf_put_utf8(&b, j->err.p, n > 300 ? 300 : n);
    return buf_cstr(&b);
}

void remotes_on_job(remotes_t *rs, runner_t *r, const job_t *j, int64_t now)
{
    if (j->tag < 0 || (size_t)j->tag >= rs->n) return;
    remote_t *rm = &rs->list[j->tag];
    rm->busy = false;
    if (j->killed) return;
    char *line = last_line(&j->out);
    int code = WIFEXITED(j->status) ? WEXITSTATUS(j->status) : -1;
    char why[512];

    if (!strncmp(line, "OK ", 3)) {
        cJSON *p = cJSON_Parse(line + 3);
        const cJSON *ll = cJSON_GetObjectItemCaseSensitive(p, "landlock");
        if (cJSON_IsNumber(ll)) {
            rm->landlock = (int)ll->valuedouble;
            set_state(rs, rm, RM_READY, NULL);
            rm->next_at = now + RECHECK_MS;
            log_info("服务器 %s 就绪（%s）", rm->conf->name,
                     rm->landlock > 0 ? "有 Landlock 沙箱" : "没有沙箱，自动模式下的 Shell 也需要审批");
        } else {
            snprintf(why, sizeof why, "执行器的自检结果看不懂：%.*s", (int)utf8_cut(line + 3, strlen(line + 3), 200), line + 3);
            remote_failed(rs, rm, why, now);
        }
        cJSON_Delete(p);
    } else if (!strncmp(line, "NEED ", 5)) {
        /* 服务器上还没有能用的执行器 */
        char *save = NULL;
        char *arch = strtok_r(line + 5, " ", &save);
        char *cc = strtok_r(NULL, " ", &save);
        free(rm->arch);
        free(rm->cc);
        rm->arch = xstrdup(arch ? arch : "");
        rm->cc = cc && *cc ? xstrdup(cc) : NULL;
        if (rm->conf->exec) {
            snprintf(why, sizeof why, "服务器上的执行器 %s 不存在或者运行不了", rm->conf->exec);
            remote_failed(rs, rm, why, now);
        } else if (!strcmp(rm->arch, rs->arch)) {
            start_upload(rs, rm, r, now);
        } else if (rm->cc && rs->src_tar) {
            start_build(rs, rm, r, now);
        } else {
            snprintf(why, sizeof why, "服务器是 %s，本机的执行器是 %s，%s", rm->arch, rs->arch,
                     rs->src_tar ? "服务器上又没有 C 编译器（cc），没法现编。请在服务器上编译 nano-exec，并在 remote 那一行写上它的路径"
                                 : "传不过去。请在服务器上编译 nano-exec，并在 remote 那一行写上它的路径");
            remote_failed(rs, rm, why, now);
        }
    } else if (!strncmp(line, "FAIL", 4)) {
        const char *msg = line[4] ? line + 5 : "";
        if (rm->state == RM_UPLOAD && rm->cc && rs->src_tar) {
            log_info("服务器 %s 跑不了上传的执行器（%.*s），改为在服务器上编译", rm->conf->name, (int)utf8_cut(msg, strlen(msg), 200), msg);
            start_build(rs, rm, r, now);
        } else if (rm->state == RM_UPLOAD) {
            snprintf(why, sizeof why, "上传的执行器在服务器上跑不起来（%.*s）。服务器上装个 C 编译器（cc）就能现编，"
                     "或者在本机装上 glibc-static / musl 后重新 make，传静态版本", (int)utf8_cut(msg, strlen(msg), 200), msg);
            remote_failed(rs, rm, why, now);
        } else {
            snprintf(why, sizeof why, "在服务器上编译执行器失败：%.*s", (int)utf8_cut(msg, strlen(msg), 300), msg);
            remote_failed(rs, rm, why, now);
        }
    } else {
        char *e = first_err(j);
        if (code == 255) snprintf(why, sizeof why, "连不上：%s", e);
        else snprintf(why, sizeof why, "%s失败（退出码 %d）：%s", state_name(rm->state), code, e);
        free(e);
        remote_failed(rs, rm, why, now);
    }
    free(line);
}

cJSON *remotes_json(const remotes_t *rs)
{
    cJSON *arr = cJSON_CreateArray();
    for (size_t i = 0; i < rs->n; i++) {
        const remote_t *rm = &rs->list[i];
        cJSON *o = cJSON_CreateObject();
        cJSON_AddStringToObject(o, "name", rm->conf->name);
        cJSON_AddStringToObject(o, "target", rm->conf->target);
        cJSON_AddBoolToObject(o, "ok", rm->state == RM_READY);
        cJSON_AddNumberToObject(o, "landlock", rm->landlock);
        const char *d = rm->state == RM_FAILED ? rm->detail : rm->state == RM_READY ? NULL : state_name(rm->state);
        if (d) cJSON_AddStringToObject(o, "detail", d);
        cJSON_AddItemToArray(arr, o);
    }
    return arr;
}
