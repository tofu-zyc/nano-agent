/* nexec_main.c —— nano-exec 入口：读请求，准备目录和沙箱，执行工具，输出结果。见 nexec.h */
#include "nexec.h"
#include "plat.h"
#include "sandbox.h"

#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if !NA_WINDOWS
#include <dirent.h>
#include <sys/stat.h>
#if !NA_MACOS
#include <sys/syscall.h>
#endif
#include <unistd.h>
#endif

#define NEXEC_VERSION 1
#define REQ_MAX (64u << 20)

#if !NA_WINDOWS
extern char **environ;
#endif

/* 沙箱里始终只读可见的系统路径（不存在的会被跳过） */
static const char *const SYSTEM_RO[] = {
    "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt",
    "/proc", "/sys", "/nix/store", "/run/current-system", "/snap",
    "/dev/zero", "/dev/random", "/dev/urandom",
};
static const char *const SYSTEM_RW[] = { "/dev/null", "/dev/full" };

typedef struct {
    const char *name;
    bool (*fn)(const xctx *, const cJSON *, buf_t *);
} tool_entry;

static const tool_entry TOOLS[] = {
    { "Read", tool_read }, { "Write", tool_write }, { "Edit", tool_edit },
    { "Grep", tool_grep }, { "Shell", tool_shell },
};

/* ---- 输出 ---- */

static void respond(bool ok, const char *content)
{
    cJSON *r = cJSON_CreateObject();
    cJSON_AddBoolToObject(r, "ok", ok);
    cJSON_AddStringToObject(r, "content", content);
    char *s = cJSON_PrintUnformatted(r);
    if (s) {
        fputs(s, stdout);
        fputc('\n', stdout);
        fflush(stdout);
    }
    cJSON_free(s);
    cJSON_Delete(r);
}

/* ---- 准备 ---- */

static void free_list(char **v, size_t n)
{
    for (size_t i = 0; i < n; i++) free(v[i]);
    free(v);
}

static void xctx_free(xctx *x)
{
    free_list(x->dirs, x->ndirs);
    free_list(x->ro, x->nro);
    free_list(x->rw, x->nrw);
    if (x->env) {
        for (char **e = x->env; *e; e++) free(*e);
        free(x->env);
    }
    free(x->home);
    free(x->tmp);
    free(x->cache);
    memset(x, 0, sizeof *x);
}

/* 读一行请求。内核只发这一行，之后 stdin 保持打开，关闭表示取消。 */
static char *read_request(void)
{
    buf_t b = {0};
    char tmp[65536];
    while (fgets(tmp, sizeof tmp, stdin)) {
        size_t n = strlen(tmp);
        buf_append(&b, tmp, n);
        if (memchr(tmp, '\n', n)) break;
        if (b.len > REQ_MAX) break;
    }
    if (!b.len || b.len > REQ_MAX) {
        buf_free(&b);
        return NULL;
    }
    return buf_cstr(&b);
}

static void push(char ***v, size_t *n, char *s)
{
    *v = xrealloc(*v, (*n + 2) * sizeof **v);
    (*v)[(*n)++] = s;
    (*v)[*n] = NULL;
}

/* 读字符串数组，展开 ~ 并解析符号链接。must_exist 时缺失的目录算错误，否则跳过。
 * all_drives：Windows 上把 "/" 当成所有盘（管理员读全盘 / 提权时 rw "/"）。 */
static int load_paths(const xctx *x, const cJSON *arr, bool must_exist, bool all_drives,
                      char ***out, size_t *n, buf_t *err)
{
    (void)x;
    const cJSON *it;
    cJSON_ArrayForEach(it, arr) {
        if (!cJSON_IsString(it) || !*it->valuestring) continue;
        if (all_drives && NA_WINDOWS &&
            (!strcmp(it->valuestring, "/") || !strcmp(it->valuestring, "\\"))) {
            size_t nd = 0;
            char **drv = plat_drives(&nd);
            for (size_t i = 0; i < nd; i++) push(out, n, drv[i]);
            free(drv);
            continue;
        }
        char *p = plat_expand_home(it->valuestring);
        char *real = plat_canonical(p);
        plat_stat_t st;
        if (!real || (must_exist && (!plat_stat(real, &st) || !st.is_dir))) {
            if (must_exist) {
                buf_puts(err, "工作目录不存在或不是目录：");
                buf_puts(err, it->valuestring);
                buf_puts(err, "。请让用户用 /dir 检查工作目录设置");
                free(p);
                free(real);
                return -1;
            }
            free(p);
            free(real);
            continue;
        }
        free(p);
        push(out, n, real);
    }
    return 0;
}

/* tmp_key 只保留安全字符，用作目录名 */
static char *safe_key(const char *k)
{
    if (!k || !*k) return xstrdup("default");
    char *s = xstrdup(k);
    for (char *p = s; *p; p++)
        if (!((*p >= 'a' && *p <= 'z') || (*p >= 'A' && *p <= 'Z') || (*p >= '0' && *p <= '9') ||
              *p == '-' || *p == '_' || *p == '.'))
            *p = '_';
    if (s[0] == '.') s[0] = '_';
    return s;
}

static int setup(xctx *x, const cJSON *req, buf_t *err)
{
    memset(x, 0, sizeof *x);
    x->home = plat_home();
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(req, "max_output");
    x->max_output = cJSON_IsNumber(v) && v->valuedouble >= 1024 && v->valuedouble <= (64 << 20)
                  ? (size_t)v->valuedouble : 64 * 1024;
    x->write = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(req, "write"));
    x->home_only = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(req, "home"));
    x->watch_stdin = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(req, "watch_stdin"));

    if (load_paths(x, cJSON_GetObjectItemCaseSensitive(req, "dirs"), true, false, &x->dirs, &x->ndirs, err) < 0 ||
        load_paths(x, cJSON_GetObjectItemCaseSensitive(req, "ro"), false, true, &x->ro, &x->nro, err) < 0 ||
        load_paths(x, cJSON_GetObjectItemCaseSensitive(req, "rw"), false, true, &x->rw, &x->nrw, err) < 0)
        return -1;
    /* 可写的目录不能包含执行器自己：改写了它，下一次调用就不在沙箱里了。
     * 本机由内核把关；远程时执行器放在服务器的 ~/.nano-agent 里，工作目录是 ~ 的话会被包含进来 */
    char *self = plat_self_exe();
    for (size_t i = 0; self && i < x->ndirs; i++) {
        const char *d = x->dirs[i];
        if (x->write && plat_inside(self, d)) {
            buf_puts(err, "工作目录 ");
            buf_puts(err, d);
            buf_puts(err, " 包含执行器自己（");
            buf_puts(err, self);
            buf_puts(err, "），不能用作可写的工作目录。请让用户换一个更具体的目录");
            free(self);
            return -1;
        }
    }
    free(self);

    const char *nh = getenv("NANO_HOME");
    char *base = nh && plat_is_abs(nh) ? xstrdup(nh) : plat_join(x->home, ".nano-agent");
    char *key = safe_key(cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(req, "tmp_key")));
    char *tmpbase = plat_join(base, "tmp");
    x->tmp = plat_join(tmpbase, key);
    x->cache = plat_join(base, "cache");
    free(tmpbase);
    free(base);
    free(key);
    if (plat_makedirs(x->tmp) < 0 || plat_makedirs(x->cache) < 0) {
        buf_puts(err, "创建临时目录失败：");
        buf_puts(err, strerror(errno));
        return -1;
    }
    x->env = plat_build_env(x->home, x->tmp, x->cache);
    return 0;
}

#if !NA_WINDOWS
/* PATH 里的目录（以及旁边的 lib，比如 nvm 装的 node）只读可见，用户自己装的命令才能用。
 * /mnt 下的不加：WSL 会把 Windows 的目录放进 PATH，那里的程序不受沙箱限制。 */
static void add_path_dirs(const xctx *x, char ***ro, size_t *nro)
{
    const char *path = NULL;
    for (char **e = x->env; *e; e++)
        if (!strncmp(*e, "PATH=", 5)) path = *e + 5;
    if (!path) return;
    char *dup = xstrdup(path), *save = NULL;
    for (char *d = strtok_r(dup, ":", &save); d; d = strtok_r(NULL, ":", &save)) {
        if (d[0] != '/' || !strcmp(d, "/") || plat_inside(d, "/mnt")) continue;
        push(ro, nro, xstrdup(d));
        size_t l = strlen(d);
        if (l > 4 && !strcmp(d + l - 4, "/bin")) push(ro, nro, xasprintf("%.*s/lib", (int)(l - 4), d));
    }
    free(dup);
}
#endif

static int enter_sandbox(const xctx *x, buf_t *err)
{
    char **rw = NULL, **ro = NULL;
    size_t nrw = 0, nro = 0;
    for (size_t i = 0; i < x->ndirs; i++) push(x->write ? &rw : &ro, x->write ? &nrw : &nro, xstrdup(x->dirs[i]));
    for (size_t i = 0; i < x->nrw; i++) push(&rw, &nrw, xstrdup(x->rw[i]));
    for (size_t i = 0; i < x->nro; i++) push(&ro, &nro, xstrdup(x->ro[i]));
    push(&rw, &nrw, xstrdup(x->tmp));
    push(&rw, &nrw, xstrdup(x->cache));
    for (size_t i = 0; i < sizeof SYSTEM_RW / sizeof *SYSTEM_RW; i++) push(&rw, &nrw, xstrdup(SYSTEM_RW[i]));
    for (size_t i = 0; i < sizeof SYSTEM_RO / sizeof *SYSTEM_RO; i++) push(&ro, &nro, xstrdup(SYSTEM_RO[i]));
#if !NA_WINDOWS
    add_path_dirs(x, &ro, &nro);
    /* /etc/resolv.conf 常常是指向 /run 或 /mnt/wsl 的链接，只放行它指向的那个文件 */
    char *resolv = realpath("/etc/resolv.conf", NULL);
    if (resolv) push(&ro, &nro, resolv);
#endif

    sb_rules r = { .rw = (const char *const *)rw, .nrw = nrw, .ro = (const char *const *)ro, .nro = nro };
    char msg[512];
    int rc = sb_apply(&r, msg, sizeof msg);
    if (rc < 0) {
        buf_puts(err, "沙箱启动失败，没有执行：");
        buf_puts(err, msg);
    }
    for (size_t i = 0; i < nrw; i++) free(rw[i]);
    for (size_t i = 0; i < nro; i++) free(ro[i]);
    free(rw);
    free(ro);
    return rc < 0 ? -1 : 0;
}

/* ---- 入口 ---- */

static int probe(void)
{
    char *home = plat_home();
    xctx x = { .env = plat_build_env(home, home, home) };
    char *rg = rg_path(&x);
    cJSON *r = cJSON_CreateObject();
    cJSON_AddNumberToObject(r, "v", NEXEC_VERSION);
    cJSON_AddNumberToObject(r, "landlock", sb_landlock_abi());
    cJSON_AddStringToObject(r, "os", plat_os());
    cJSON_AddStringToObject(r, "arch", plat_arch());
    cJSON_AddBoolToObject(r, "rg", rg != NULL);
    char *s = cJSON_PrintUnformatted(r);
    puts(s);
    cJSON_free(s);
    cJSON_Delete(r);
    free(rg);
    for (char **e = x.env; *e; e++) free(*e);
    free(x.env);
    free(home);
    return 0;
}

/* --ls 路径：列出一个目录下的子目录（远程选工作目录用），格式和内核的 ls_result 一样。
 * 包含执行器自己的目录标成 blocked。只返回目录名，不读任何文件内容。
 * Windows 上请求 / 或空路径时返回盘符列表（C:\、D:\）。 */
#define LS_MAX 1000

static int cmp_name(const void *a, const void *b)
{
    return strcmp(*(char *const *)a, *(char *const *)b);
}

static int list_dir(const char *arg)
{
    char *home = plat_home();
    bool roots = NA_WINDOWS && (!arg || !*arg || !strcmp(arg, "/") || !strcmp(arg, "\\"));
    char *p = roots ? xstrdup(home) : plat_expand_home(arg && *arg ? arg : "~");
    char *self = plat_self_exe();
    cJSON *r = cJSON_CreateObject();
    cJSON_AddStringToObject(r, "home", home);

    if (roots) {
        size_t nr = 0;
        char **v = plat_drives(&nr);
        cJSON_AddBoolToObject(r, "ok", true);
        cJSON_AddStringToObject(r, "path", "");
        cJSON *arr = cJSON_AddArrayToObject(r, "entries");
        for (size_t i = 0; i < nr; i++) {
            cJSON *it = cJSON_CreateObject();
            cJSON_AddStringToObject(it, "name", v[i]);
            cJSON_AddItemToArray(arr, it);
        }
        plat_drives_free(v, nr);
    } else {
        char *real = plat_canonical(p);
        plat_dirent *ents = NULL;
        size_t n = 0;
        bool more = false;
        char err[256] = "";
        if (!real || !plat_read_dir(real, LS_MAX, &ents, &n, &more, err, sizeof err)) {
            cJSON_AddBoolToObject(r, "ok", false);
            cJSON_AddStringToObject(r, "detail", !plat_is_abs(p) ? "要写绝对路径或 ~ 开头"
                                       : real ? err : "目录不存在或不可访问");
        } else {
            cJSON_AddBoolToObject(r, "ok", true);
            cJSON_AddStringToObject(r, "path", real);
            cJSON_AddBoolToObject(r, "blocked", self && plat_inside(self, real));
            char **names = NULL;
            size_t nd = 0;
            for (size_t i = 0; i < n; i++) {
                if (!ents[i].dir) continue;
                names = xrealloc(names, (nd + 1) * sizeof *names);
                names[nd++] = xstrdup(ents[i].name);
            }
            qsort(names, nd, sizeof *names, cmp_name);
            cJSON *arr = cJSON_AddArrayToObject(r, "entries");
            for (size_t i = 0; i < nd; i++) {
                cJSON *it = cJSON_CreateObject();
                buf_t b = {0};
                buf_put_utf8(&b, names[i], strlen(names[i]));
                cJSON_AddStringToObject(it, "name", buf_cstr(&b));
                buf_free(&b);
                char *sub = plat_join(real, names[i]);
                if (self && plat_inside(self, sub)) cJSON_AddBoolToObject(it, "blocked", true);
                free(sub);
                cJSON_AddItemToArray(arr, it);
                free(names[i]);
            }
            free(names);
            if (more) cJSON_AddBoolToObject(r, "truncated", true);
        }
        plat_dirents_free(ents, n);
        free(real);
    }
    char *s = cJSON_PrintUnformatted(r);
    puts(s);
    cJSON_free(s);
    cJSON_Delete(r);
    free(p);
    free(self);
    free(home);
    return 0;
}

#if !NA_WINDOWS
static void close_inherited_fds(void)
{
#ifdef SYS_close_range
    if (syscall(SYS_close_range, 3U, ~0U, 0U) == 0) return;
#endif
    for (int fd = 3; fd < 1024; fd++) close(fd);
}
#endif

int main(int argc, char **argv)
{
    plat_init();
    int nargs = 0;
    char **cmdline = plat_args(argc, argv, &nargs);
    if (nargs > 1 && !strcmp(cmdline[1], "--probe")) return probe();
    if (nargs > 1 && !strcmp(cmdline[1], "--ls")) return list_dir(nargs > 2 ? cmdline[2] : "~");
    if (nargs > 1) {
        fprintf(stderr, "用法：%s [--probe | --ls 目录]\n  从 stdin 读一行请求 JSON，向 stdout 写一行结果 JSON\n",
                nargs > 0 ? cmdline[0] : "nano-exec");
        return 2;
    }
#if !NA_WINDOWS
    close_inherited_fds();
    signal(SIGPIPE, SIG_IGN);
#endif

    char *line = read_request();
    if (!line) {
        respond(false, "执行器没有收到请求");
        return 1;
    }
    cJSON *req = cJSON_Parse(line);
    free(line);
    if (!cJSON_IsObject(req)) {
        cJSON_Delete(req);
        respond(false, "执行器收到的请求不是合法的 JSON 对象");
        return 1;
    }

    const char *name = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(req, "tool"));
    const cJSON *args = cJSON_GetObjectItemCaseSensitive(req, "args");
    const tool_entry *t = NULL;
    for (size_t i = 0; name && i < sizeof TOOLS / sizeof *TOOLS; i++)
        if (!strcmp(TOOLS[i].name, name)) t = &TOOLS[i];

    buf_t out = {0};
    bool ok = false;
    xctx x = {0};
    if (!t) {
        buf_puts(&out, "未知工具：");
        buf_puts(&out, name ? name : "（空）");
    } else if (args && !cJSON_IsObject(args)) {
        buf_puts(&out, "参数应为 JSON 对象");
    } else if (setup(&x, req, &out) == 0 && enter_sandbox(&x, &out) == 0) {
        cJSON *empty = args ? NULL : cJSON_CreateObject();
        ok = t->fn(&x, args ? args : empty, &out);
        cJSON_Delete(empty);
    }
    if (!out.len) buf_puts(&out, "（无输出）");
    respond(ok, buf_cstr(&out));
    buf_free(&out);
    xctx_free(&x);
    cJSON_Delete(req);
    return 0;
}
