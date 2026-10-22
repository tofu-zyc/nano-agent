/* main.c —— nano-kernel：连接网关，驱动状态机，调执行器执行工具
 *
 * 单线程 poll 循环：网关连接、执行器的管道、信号唤醒管道都在同一个 poll 里。
 * 外部消息 → 事件 → na_core_feed；na_core_next 取出的命令 → 发给网关或交给执行器。
 */
#define _XOPEN_SOURCE 700   /* nftw */
#include "cJSON.h"
#include "conf.h"
#include "core.h"
#include "link.h"
#include "runner.h"
#include "tools.h"
#include "util.h"

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <ftw.h>
#include <limits.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define PROTO_VERSION      7
#define PING_INTERVAL_MS   30000
#define DEAD_AFTER_MS      90000    /* 这么久没收到网关任何消息就认为连接已死 */
#define CONNECT_TIMEOUT_MS 5000
#define MAX_BACKOFF_MS     30000
#define MAX_POLL           1024

static volatile sig_atomic_t g_stop;
static int g_wake[2] = { -1, -1 };   /* 信号处理函数往这里写一个字节，唤醒 poll */

static void on_signal(int sig)
{
    int saved = errno;
    if (sig == SIGINT || sig == SIGTERM) g_stop = 1;
    char c = (char)sig;
    if (write(g_wake[1], &c, 1) < 0) { /* 管道满了也无妨，poll 已经会醒 */ }
    errno = saved;
}

/* 交给网关执行的工具调用（工作目录不在这台机器上，或者 Skill），等 exec_result */
typedef struct gwjob {
    char *sid, *id;
    struct gwjob *next;
} gwjob_t;

/* 执行器任务的种类：自己这一轮的工具调用，或者网关请这台机器代为执行的（run） */
enum { JOB_TOOL = 0, JOB_HOST = 1 };

typedef struct {
    kconf           conf;
    const na_tool_policy *tools;
    size_t          ntools;
    int             landlock;    /* 执行器探测到的 Landlock 版本，0 表示没有沙箱 */
    bool            rg;
    char           *exec_real;   /* 执行器的真实路径，它不能落在工作目录里 */
    na_core        *core;
    runner_t        runner;
    struct gwjob   *gwjobs;      /* 交给网关执行、还没有结果的工具调用 */
    link_t          link;
    int64_t         last_ping;
    int64_t         next_connect;
    int             backoff;
} kernel_t;

/* ---- 协议辅助 ---- */

static const char *sget(const cJSON *o, const char *key)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(o, key);
    return cJSON_IsString(v) ? v->valuestring : NULL;
}

static cJSON *msg_new(const char *type, const char *sid)
{
    cJSON *m = cJSON_CreateObject();
    cJSON_AddStringToObject(m, "t", type);
    if (sid) cJSON_AddStringToObject(m, "sid", sid);
    return m;
}

static void send_hello(kernel_t *k)
{
    cJSON *m = msg_new("hello", NULL);
    cJSON_AddNumberToObject(m, "v", PROTO_VERSION);
    cJSON_AddStringToObject(m, "token", k->conf.token);
    if (!*k->conf.token) cJSON_AddBoolToObject(m, "pair", true);
    cJSON_AddStringToObject(m, "node", k->conf.node);
    cJSON_AddItemToObject(m, "tools", tools_specs());
    cJSON *sb = cJSON_AddObjectToObject(m, "sandbox");
    cJSON_AddNumberToObject(sb, "landlock", k->landlock);
    link_send(&k->link, m);
}

/* ---- 命令 → 外部动作 ---- */

/* real（已解析符号链接）是不是包含内核自己：配置目录或执行器 */
static bool holds_self(const kernel_t *k, const char *real)
{
    size_t n = strlen(real);
    const char *mine[] = { k->conf.dir, k->exec_real };
    for (size_t i = 0; i < 2; i++) {
        const char *m = mine[i];
        if (m && (n == 1 || (!strncmp(m, real, n) && (m[n] == '/' || !m[n])))) return true;
    }
    return false;
}

/* 工作目录不能包含内核自己：配置里有 token，执行器要是能被改写，沙箱就形同虚设 */
static bool guard_paths(const kernel_t *k, const cJSON *list, char *err, size_t errlen)
{
    const cJSON *it;
    cJSON_ArrayForEach(it, list) {
        if (!cJSON_IsString(it)) continue;
        const char *p = it->valuestring;
        const char *home = getenv("HOME");
        char *full = p[0] == '~' && (p[1] == '/' || !p[1]) && home ? xasprintf("%s%s", home, p + 1) : xstrdup(p);
        char *real = realpath(full, NULL);
        free(full);
        if (!real) continue;   /* 不存在的目录交给执行器报错 */
        bool bad = holds_self(k, real);
        free(real);
        if (bad) {
            snprintf(err, errlen, "工作目录 %s 包含 nano-agent 自己（内核配置里有密钥，执行器也可能被改写），"
                     "不能用作工作目录。请让用户换一个目录，或者把 nano-agent 装到工作目录之外", p);
            return false;
        }
    }
    return true;
}

/* 列出一个目录下的子目录，给网页上选工作目录用。只返回目录名，不读任何文件内容；
 * 包含内核自己的目录标成 blocked（不能用作工作目录）。 */
#define LS_MAX 1000

static int cmp_name(const void *a, const void *b)
{
    return strcmp(*(char *const *)a, *(char *const *)b);
}

static void handle_ls(kernel_t *k, const cJSON *m)
{
    const char *id = sget(m, "id"), *p = sget(m, "path");
    const char *home = getenv("HOME");
    if (!home || home[0] != '/') home = "/";
    char *full = !p || !*p || !strcmp(p, "~") ? xstrdup(home)
               : p[0] == '~' && p[1] == '/' ? xasprintf("%s%s", home, p + 1)
               : xstrdup(p);
    cJSON *r = msg_new("ls_result", NULL);
    cJSON_AddStringToObject(r, "id", id ? id : "");
    cJSON_AddStringToObject(r, "home", home);
    char *real = full[0] == '/' ? realpath(full, NULL) : NULL;
    DIR *d = real ? opendir(real) : NULL;
    if (!d) {
        cJSON_AddBoolToObject(r, "ok", false);
        cJSON_AddStringToObject(r, "detail", full[0] != '/' ? "要写绝对路径或 ~ 开头" : strerror(errno));
    } else {
        cJSON_AddBoolToObject(r, "ok", true);
        cJSON_AddStringToObject(r, "path", real);
        cJSON_AddBoolToObject(r, "blocked", holds_self(k, real));
        char **names = NULL;
        size_t n = 0;
        bool more = false;
        struct dirent *e;
        while ((e = readdir(d))) {
            if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
            bool dir = e->d_type == DT_DIR;
            if (e->d_type == DT_UNKNOWN || e->d_type == DT_LNK) {
                struct stat st;
                dir = fstatat(dirfd(d), e->d_name, &st, 0) == 0 && S_ISDIR(st.st_mode);
            }
            if (!dir) continue;
            if (n >= LS_MAX) { more = true; continue; }
            names = xrealloc(names, (n + 1) * sizeof *names);
            names[n++] = xstrdup(e->d_name);
        }
        closedir(d);
        qsort(names, n, sizeof *names, cmp_name);
        cJSON *arr = cJSON_AddArrayToObject(r, "entries");
        for (size_t i = 0; i < n; i++) {
            cJSON *it = cJSON_CreateObject();
            buf_t b = {0};
            buf_put_utf8(&b, names[i], strlen(names[i]));
            cJSON_AddStringToObject(it, "name", buf_cstr(&b));
            buf_free(&b);
            char *sub = xasprintf("%s/%s", strcmp(real, "/") ? real : "", names[i]);
            if (holds_self(k, sub)) cJSON_AddBoolToObject(it, "blocked", true);
            free(sub);
            cJSON_AddItemToArray(arr, it);
            free(names[i]);
        }
        free(names);
        if (more) cJSON_AddBoolToObject(r, "truncated", true);
    }
    free(real);
    free(full);
    link_send(&k->link, r);
}

static void gw_add(kernel_t *k, const char *sid, const char *id)
{
    gwjob_t *g = xcalloc(1, sizeof *g);
    g->sid = xstrdup(sid ? sid : "");
    g->id = xstrdup(id ? id : "");
    g->next = k->gwjobs;
    k->gwjobs = g;
}

static void gw_free(gwjob_t *g)
{
    free(g->sid);
    free(g->id);
    free(g);
}

/* 取出（并摘下）一个交给网关的调用；没有时返回 NULL（已经超时或取消了） */
static gwjob_t *gw_take(kernel_t *k, const char *sid, const char *id)
{
    for (gwjob_t **p = &k->gwjobs; *p; p = &(*p)->next) {
        gwjob_t *g = *p;
        if (!strcmp(g->sid, sid ? sid : "") && !strcmp(g->id, id ? id : "")) {
            *p = g->next;
            return g;
        }
    }
    return NULL;
}

static void tool_done(kernel_t *k, const char *sid, const char *id, bool ok, const char *content, int64_t now)
{
    na_event ev = { .type = NA_EV_TOOL_DONE, .sid = sid, .id = id, .ok = ok, .content = content };
    na_core_feed(k->core, now, &ev);
}

/* ---- 工作目录 ----
 *
 * 一个会话可以同时挂好几个工作目录，可能在不同的机器上。input 的 workspaces 每项是
 * {name, dirs, ro, rw, local, landlock}：name 是给模型看的名字（机器:路径），local 表示在这台机器上
 * （内核自己执行），否则交给网关在那台机器上执行。工具参数里的 workspace 指定用哪个。 */

static void ws_names(const cJSON *list, char *out, size_t outlen)
{
    buf_t b = {0};
    const cJSON *it;
    cJSON_ArrayForEach(it, list) {
        const char *n = sget(it, "name");
        buf_puts(&b, b.len ? "、" : "");
        buf_puts(&b, n ? n : "?");
    }
    char *s = buf_cstr(&b);   /* 接管了 b 的内存 */
    snprintf(out, outlen, "%s", s);
    free(s);
}

/* 名字里冒号前面是机器，后面是路径 */
static bool ws_part_is(const char *name, const char *want, bool machine)
{
    const char *colon = strchr(name, ':');
    if (!colon) return false;
    return machine ? (size_t)(colon - name) == strlen(want) && !strncmp(name, want, (size_t)(colon - name))
                   : !strcmp(colon + 1, want);
}

/* 按参数里的 workspace 找挂载的工作目录：完整的名字、序号（从 1 开始）、只写机器或只写路径（唯一时）。
 * 只挂了一个时可以不写。找不到返回 NULL，原因写进 err（直接作为工具结果给模型看）。 */
static const cJSON *pick_workspace(const cJSON *policy, const cJSON *args, char *err, size_t errlen)
{
    const cJSON *list = cJSON_GetObjectItemCaseSensitive(policy, "workspaces");
    int n = cJSON_IsArray(list) ? cJSON_GetArraySize(list) : 0;
    char names[600];
    if (n == 0) {
        snprintf(err, errlen, "这个会话没有挂载工作目录，不能用工具");
        return NULL;
    }
    ws_names(list, names, sizeof names);
    const cJSON *want = cJSON_GetObjectItemCaseSensitive(args, "workspace");
    if (!want || cJSON_IsNull(want) || (cJSON_IsString(want) && !*want->valuestring)) {
        if (n == 1) return cJSON_GetArrayItem(list, 0);
        snprintf(err, errlen, "这个会话挂了 %d 个工作目录，用 workspace 参数指明在哪个上执行：%s", n, names);
        return NULL;
    }
    if (cJSON_IsNumber(want)) {
        int i = want->valueint;
        if (i >= 1 && i <= n) return cJSON_GetArrayItem(list, i - 1);
    } else if (cJSON_IsString(want)) {
        const char *w = want->valuestring;
        const cJSON *it, *hit = NULL;
        int hits = 0;
        cJSON_ArrayForEach(it, list)
            if (sget(it, "name") && !strcmp(sget(it, "name"), w)) return it;
        char *end;
        long i = strtol(w, &end, 10);
        if (!*end && i >= 1 && i <= n) return cJSON_GetArrayItem(list, (int)i - 1);
        for (int pass = 0; pass < 2 && !hits; pass++) {
            cJSON_ArrayForEach(it, list) {
                if (sget(it, "name") && ws_part_is(sget(it, "name"), w, pass == 0)) {
                    hit = it;
                    hits++;
                }
            }
        }
        if (hits == 1) return hit;
    }
    char *shown = cJSON_PrintUnformatted(want);
    snprintf(err, errlen, "没有工作目录 %s。这个会话挂载的工作目录：%s", shown ? shown : "?", names);
    cJSON_free(shown);
    return NULL;
}

/* 交给网关：Skill（技能在网关上），或者工作目录不在这台机器上的工具调用 */
static void send_exec(kernel_t *k, const na_cmd *c, cJSON *args, const char *workspace)
{
    cJSON *m = msg_new("exec", c->sid);
    cJSON_AddStringToObject(m, "id", c->id);
    cJSON_AddStringToObject(m, "name", c->name);
    cJSON_AddItemToObject(m, "args", args ? args : cJSON_CreateString(c->args ? c->args : ""));
    if (workspace) cJSON_AddStringToObject(m, "workspace", workspace);
    if (c->elevated) cJSON_AddBoolToObject(m, "elevated", true);
    cJSON_AddNumberToObject(m, "timeout_ms", c->timeout_ms);
    cJSON_AddNumberToObject(m, "max_output", (double)k->conf.max_output);
    link_send(&k->link, m);
    gw_add(k, c->sid, c->id);
    log_info("交给网关执行 %s（%s）%s%s", c->name, c->id, workspace ? "：" : "", workspace ? workspace : "");
}

/* 启动执行器。req 是请求（接管所有权），补上执行器需要的通用字段 */
static job_t *start_exec(kernel_t *k, cJSON *req, const char *sid, const char *id, const char *name,
                         char *err, size_t errlen)
{
    bool ok = true;
    /* 只检查工作目录：ro 只读放行；rw 只有提权时才会出现 /，已经过用户审批 */
    for (const char *const *key = (const char *const[]){ "dirs", NULL }; *key && ok; key++)
        ok = guard_paths(k, cJSON_GetObjectItemCaseSensitive(req, *key), err, errlen);
    job_t *j = NULL;
    if (ok) {
        if (!cJSON_GetObjectItemCaseSensitive(req, "max_output"))
            cJSON_AddNumberToObject(req, "max_output", (double)k->conf.max_output);
        cJSON_DeleteItemFromObjectCaseSensitive(req, "watch_stdin");
        cJSON_AddBoolToObject(req, "watch_stdin", true);
        char *body = cJSON_PrintUnformatted(req);
        char *line = xasprintf("%s\n", body);
        cJSON_free(body);
        char *argv[] = { k->conf.exec, NULL };
        j = runner_start_ex(&k->runner, argv, sid, id, name, line, strlen(line), err, errlen);
    }
    cJSON_Delete(req);
    return j;
}

static void exec_tool(kernel_t *k, const na_cmd *c, int64_t now)
{
    char err[1024] = "";
    cJSON *args = cJSON_Parse(c->args ? c->args : "{}");
    if (!strcmp(c->name, "Skill") || !strcmp(c->name, "Gateway") || !strcmp(c->name, "GatewayAdmin")) {
        send_exec(k, c, args, NULL);
        return;
    }
    cJSON *pol = cJSON_Parse(c->policy ? c->policy : "{}");
    const cJSON *ws = pick_workspace(pol, args, err, sizeof err);
    job_t *j = NULL;
    if (ws) {
        cJSON_DeleteItemFromObjectCaseSensitive(args, "workspace");
        if (!cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(ws, "local"))) {
            /* 工作目录不在这台机器上：交给网关，约束由网关按会话的设置生成 */
            send_exec(k, c, args, sget(ws, "name"));
            cJSON_Delete(pol);
            return;
        }
        cJSON *req = cJSON_CreateObject();
        for (const char *const *key = (const char *const[]){ "dirs", "ro", "rw", NULL }; *key; key++) {
            const cJSON *v = cJSON_GetObjectItemCaseSensitive(ws, *key);
            cJSON_AddItemToObject(req, *key, cJSON_IsArray(v) ? cJSON_Duplicate(v, 1) : cJSON_CreateArray());
        }
        if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(ws, "home"))) cJSON_AddBoolToObject(req, "home", true);
        bool write = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(pol, "write"));
        if (c->elevated) {
            write = true;
            cJSON *rw = cJSON_GetObjectItemCaseSensitive(req, "rw");
            cJSON_AddItemToArray(rw, cJSON_CreateString("/"));
        }
        cJSON_AddBoolToObject(req, "write", write);
        cJSON_AddStringToObject(req, "tool", c->name);
        cJSON_AddItemToObject(req, "args", args);
        args = NULL;
        cJSON_AddStringToObject(req, "tmp_key", c->sid);
        j = start_exec(k, req, c->sid, c->id, c->name, err, sizeof err);
        if (j) log_info("执行 %s（%s）%s", c->name, c->id, sget(ws, "name") ? sget(ws, "name") : "");
    }
    cJSON_Delete(args);
    cJSON_Delete(pol);
    if (j) return;
    log_warn("无法执行 %s：%s", c->name, err);
    tool_done(k, c->sid, c->id, false, err, now);
}

/* ---- 网关请这台机器代为执行（工作目录在这台机器上，但这一轮由别的内核跑） ---- */

static void send_run_result(kernel_t *k, const char *id, bool ok, const char *content)
{
    cJSON *m = msg_new("run_result", NULL);
    cJSON_AddStringToObject(m, "id", id ? id : "");
    cJSON_AddBoolToObject(m, "ok", ok);
    cJSON_AddStringToObject(m, "content", content ? content : "");
    link_send(&k->link, m);
}

static void handle_run(kernel_t *k, const cJSON *m)
{
    const char *id = sget(m, "id");
    const cJSON *req = cJSON_GetObjectItemCaseSensitive(m, "req");
    const char *tool = sget(req, "tool");
    char err[1024] = "";
    job_t *j = NULL;
    if (!id || !cJSON_IsObject(req) || !tool) snprintf(err, sizeof err, "执行请求不完整");
    else j = start_exec(k, cJSON_Duplicate(req, 1), "", id, tool, err, sizeof err);
    if (!j) {
        send_run_result(k, id, false, err);
        return;
    }
    j->kind = JOB_HOST;
    log_info("代网关执行 %s（%s）", tool, id);
}

/* 技能文件放在这台机器的 ~/.nano-agent/skills/<技能名>/ 下，工具执行时只读可见 */
static bool safe_rel(const char *p)
{
    if (!p || !*p || p[0] == '/') return false;
    for (const char *s = p; *s;) {
        const char *e = strchr(s, '/');
        size_t n = e ? (size_t)(e - s) : strlen(s);
        if (n == 0 || (n == 1 && s[0] == '.') || (n == 2 && s[0] == '.' && s[1] == '.')) return false;
        s += n + (e ? 1 : 0);
    }
    return true;
}

static int unb64(int ch)
{
    if (ch >= 'A' && ch <= 'Z') return ch - 'A';
    if (ch >= 'a' && ch <= 'z') return ch - 'a' + 26;
    if (ch >= '0' && ch <= '9') return ch - '0' + 52;
    if (ch == '+') return 62;
    if (ch == '/') return 63;
    return -1;
}

static unsigned char *base64_decode(const char *s, size_t *n)
{
    size_t len = strlen(s);
    unsigned char *out = xmalloc(len / 4 * 3 + 3);
    size_t o = 0;
    unsigned v = 0;
    int bits = 0;
    for (size_t i = 0; i < len; i++) {
        if (s[i] == '=' || s[i] == '\n' || s[i] == '\r') continue;
        int d = unb64((unsigned char)s[i]);
        if (d < 0) { free(out); return NULL; }
        v = v << 6 | (unsigned)d;
        bits += 6;
        if (bits >= 8) {
            bits -= 8;
            out[o++] = (unsigned char)(v >> bits);
        }
    }
    *n = o;
    return out;
}

static int rm_entry(const char *path, const struct stat *st, int flag, struct FTW *ftw)
{
    (void)st; (void)flag; (void)ftw;
    return remove(path);
}

static bool mkdirs(char *path)
{
    for (char *p = path + 1; *p; p++) {
        if (*p != '/') continue;
        *p = 0;
        int rc = mkdir(path, 0755);
        *p = '/';
        if (rc < 0 && errno != EEXIST) return false;
    }
    return mkdir(path, 0755) == 0 || errno == EEXIST;
}

static bool put_skill(const char *name, const cJSON *files, char *dir_out, size_t dirlen, char *err, size_t errlen)
{
    const char *home = getenv("HOME");
    if (!home || home[0] != '/') { snprintf(err, errlen, "没有 HOME"); return false; }
    if (!name || !*name || strspn(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != strlen(name) || name[0] == '.') {
        snprintf(err, errlen, "技能名字不能用：%s", name ? name : "");
        return false;
    }
    const char *nh = getenv("NANO_HOME");
    char *base = nh && nh[0] == '/' ? xstrdup(nh) : xasprintf("%s/.nano-agent", home);
    char *root = xasprintf("%s/skills", base);
    free(base);
    char *dir = xasprintf("%s/%s", root, name);
    char *tmp = xasprintf("%s/.%s.tmp", root, name);
    bool ok = mkdirs(root);
    if (ok) nftw(tmp, rm_entry, 16, FTW_DEPTH | FTW_PHYS);
    ok = ok && mkdir(tmp, 0755) == 0;
    if (!ok) snprintf(err, errlen, "建不了目录 %s：%s", tmp, strerror(errno));
    const cJSON *f;
    cJSON_ArrayForEach(f, files) {
        if (!ok) break;
        const char *rel = sget(f, "path"), *data = sget(f, "data");
        if (!safe_rel(rel) || !data) {
            snprintf(err, errlen, "技能文件路径不能用：%s", rel ? rel : "");
            ok = false;
            break;
        }
        size_t n;
        unsigned char *bytes = base64_decode(data, &n);
        char *full = xasprintf("%s/%s", tmp, rel);
        char *slash = strrchr(full, '/');
        *slash = 0;
        ok = bytes && mkdirs(full);
        *slash = '/';
        int fd = ok ? open(full, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC,
                           cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(f, "exec")) ? 0755 : 0644) : -1;
        ok = fd >= 0 && (n == 0 || write(fd, bytes, n) == (ssize_t)n);
        if (fd >= 0) close(fd);
        if (!ok) snprintf(err, errlen, "写不了 %s：%s", rel, bytes ? strerror(errno) : "数据不是 base64");
        free(full);
        free(bytes);
    }
    if (ok) {
        nftw(dir, rm_entry, 16, FTW_DEPTH | FTW_PHYS);
        ok = rename(tmp, dir) == 0;
        if (!ok) snprintf(err, errlen, "放不到 %s：%s", dir, strerror(errno));
    }
    if (!ok) nftw(tmp, rm_entry, 16, FTW_DEPTH | FTW_PHYS);
    else snprintf(dir_out, dirlen, "%s", dir);
    free(root);
    free(dir);
    free(tmp);
    return ok;
}

static void handle_skill_put(kernel_t *k, const cJSON *m)
{
    char err[512] = "", dir[PATH_MAX] = "";
    bool ok = put_skill(sget(m, "skill"), cJSON_GetObjectItemCaseSensitive(m, "files"), dir, sizeof dir, err, sizeof err);
    if (ok) log_info("技能 %s 的文件放到了 %s", sget(m, "skill"), dir);
    else log_warn("放技能文件失败：%s", err);
    send_run_result(k, sget(m, "id"), ok, ok ? dir : err);
}

static void pump(kernel_t *k, int64_t now)
{
    na_cmd *c;
    while ((c = na_core_next(k->core))) {
        cJSON *m = NULL;
        switch (c->type) {
        case NA_CMD_MODEL:
            m = msg_new("model", c->sid);
            break;
        case NA_CMD_RESULT:
            m = msg_new("result", c->sid);
            cJSON_AddStringToObject(m, "id", c->id);
            cJSON_AddBoolToObject(m, "ok", c->ok);
            cJSON_AddStringToObject(m, "content", c->text ? c->text : "");
            if (!c->ok) log_info("调用 %s 没有成功：%.*s", c->id, (int)utf8_cut(c->text ? c->text : "", c->text ? strlen(c->text) : 0, 200), c->text ? c->text : "");
            break;
        case NA_CMD_APPROVE_REQ: {
            m = msg_new("approve_req", c->sid);
            cJSON_AddStringToObject(m, "id", c->id);
            cJSON_AddStringToObject(m, "name", c->name);
            cJSON *args = cJSON_Parse(c->args ? c->args : "{}");
            if (args) cJSON_AddItemToObject(m, "args", args);
            else cJSON_AddStringToObject(m, "args", c->args ? c->args : "");
            cJSON_AddStringToObject(m, "from", c->from ? c->from : "");
            log_info("等待审批 %s（%s）", c->name, c->id);
            break;
        }
        case NA_CMD_NOTICE:
            m = msg_new("notice", c->sid);
            cJSON_AddStringToObject(m, "text", c->text ? c->text : "");
            break;
        case NA_CMD_DONE:
            m = msg_new("done", c->sid);
            cJSON_AddStringToObject(m, "status", c->status);
            if (c->text) cJSON_AddStringToObject(m, "detail", c->text);
            log_info("run 结束 sid=%s status=%s", c->sid, c->status);
            break;
        case NA_CMD_EXEC:
            exec_tool(k, c, now);   /* 失败时会再喂一个事件，产生的命令由本循环继续处理 */
            break;
        case NA_CMD_KILL: {
            gwjob_t *g = gw_take(k, c->sid, c->id);
            if (g) {
                /* 交给网关执行的：请网关停下 */
                m = msg_new("exec_cancel", c->sid);
                cJSON_AddStringToObject(m, "id", c->id);
                gw_free(g);
            } else {
                runner_kill(&k->runner, c->sid, c->id);
            }
            log_info("终止 %s", c->id);
            break;
        }
        }
        if (m) link_send(&k->link, m);
        na_cmd_free(c);
    }
}

/* ---- 网关消息 → 事件 ---- */

static na_turn_reason parse_reason(const char *s)
{
    if (!s) return NA_TURN_ERROR;
    if (!strcmp(s, "final")) return NA_TURN_FINAL;
    if (!strcmp(s, "tool_use")) return NA_TURN_TOOL_USE;
    if (!strcmp(s, "refused")) return NA_TURN_REFUSED;
    return NA_TURN_ERROR;
}

static na_mode parse_mode(const char *s)
{
    if (!s) return NA_MODE_NONE;
    if (!strcmp(s, "auto")) return NA_MODE_AUTO;
    if (!strcmp(s, "ask")) return NA_MODE_ASK;
    if (!strcmp(s, "readonly")) return NA_MODE_READONLY;
    return NA_MODE_NONE;
}

/* input 里挂载的工作目录 → 这一轮的执行约束（状态机不解析，原样带着，执行时按调用取用） */
static char *build_policy(const cJSON *m, na_mode mode)
{
    cJSON *p = cJSON_CreateObject();
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, "workspaces");
    cJSON_AddItemToObject(p, "workspaces", cJSON_IsArray(v) ? cJSON_Duplicate(v, 1) : cJSON_CreateArray());
    bool home = false;
    const cJSON *it;
    if (cJSON_IsArray(v))
        cJSON_ArrayForEach(it, v) if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(it, "home"))) home = true;
    cJSON_AddBoolToObject(p, "write", !home && (mode == NA_MODE_ASK || mode == NA_MODE_AUTO));
    cJSON_AddBoolToObject(p, "elevate", cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "elevate")));
    char *s = cJSON_PrintUnformatted(p);
    cJSON_Delete(p);
    return s;
}

static void disconnect(kernel_t *k, int64_t now, const char *why);

/* 网关同意了配对：口令存进配置目录（只有自己能读），用它重新连上去 */
static void save_token(kernel_t *k, const char *token, int64_t now)
{
    if (!token || !*token || strchr(token, '\n')) return;
    int fd = open(k->conf.token_file, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
    if (fd < 0 || write(fd, token, strlen(token)) < 0 || write(fd, "\n", 1) < 0)
        log_warn("保存配对口令失败（%s），下次启动要重新配对：%s", k->conf.token_file, strerror(errno));
    if (fd >= 0) close(fd);
    free(k->conf.token);
    k->conf.token = xstrdup(token);
    k->conf.paired = true;
    log_info("网关同意了这台电脑接入，口令已保存到 %s", k->conf.token_file);
    disconnect(k, now, "配对完成，重新连接");
    k->next_connect = now;
}

static void handle_msg(kernel_t *k, const cJSON *m, int64_t now)
{
    const char *t = sget(m, "t");
    if (!t) return;
    if (!strcmp(t, "pong")) return;
    if (!strcmp(t, "welcome")) {
        k->backoff = 1000;
        log_info("网关已接受连接");
        return;
    }
    if (!strcmp(t, "error")) {
        const char *reason = sget(m, "reason") ? sget(m, "reason") : "";
        log_error("网关报错：%s", sget(m, "detail") ? sget(m, "detail") : "（无详情）");
        if (!strcmp(reason, "denied")) {
            log_error("网关没有同意这台电脑接入，内核退出。要重新申请，再启动一次即可");
            g_stop = 1;
        } else if (!strcmp(reason, "auth") && k->conf.paired) {
            log_warn("网关不认这台电脑的配对口令了（可能在网页里移除了它），重新请求配对");
            unlink(k->conf.token_file);
            k->conf.token[0] = 0;
            k->conf.paired = false;
        }
        return;
    }
    if (!strcmp(t, "pairing")) {
        k->backoff = 1000;
        log_info("等待网关确认接入：打开网关网页的「设备」页面，核对确认码 %s 后点「允许」",
                 sget(m, "code") ? sget(m, "code") : "?");
        return;
    }
    if (!strcmp(t, "paired")) {
        save_token(k, sget(m, "token"), now);
        return;
    }
    if (!strcmp(t, "ls")) {
        handle_ls(k, m);
        return;
    }
    if (!strcmp(t, "exec_result")) {
        gwjob_t *g = gw_take(k, sget(m, "sid"), sget(m, "id"));
        if (!g) return;   /* 已经超时或者被停止了 */
        bool ok = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "ok"));
        log_info("网关执行 %s %s", g->id, ok ? "完成" : "失败");
        tool_done(k, g->sid, g->id, ok, sget(m, "content") ? sget(m, "content") : "", now);
        gw_free(g);
        return;
    }
    if (!strcmp(t, "run")) {
        handle_run(k, m);
        return;
    }
    if (!strcmp(t, "run_cancel")) {
        runner_kill(&k->runner, "", sget(m, "id"));
        return;
    }
    if (!strcmp(t, "skill_put")) {
        handle_skill_put(k, m);
        return;
    }

    na_event ev = { .sid = sget(m, "sid") };
    na_call *calls = NULL;
    char **owned = NULL;
    char *policy = NULL;
    cJSON *run_pol = NULL;
    size_t ncalls = 0;

    if (!strcmp(t, "input")) {
        ev.type = NA_EV_INPUT;
        ev.from = sget(m, "from");
        ev.mode = parse_mode(sget(m, "mode"));
        ev.elevate = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "elevate"));
        policy = build_policy(m, ev.mode);
        ev.policy = policy;
        const cJSON *ws = cJSON_GetObjectItemCaseSensitive(m, "workspaces");
        log_info("run 开始 sid=%s from=%s mode=%s 工作目录 %d 个", ev.sid ? ev.sid : "-", ev.from ? ev.from : "-",
                 sget(m, "mode") ? sget(m, "mode") : "none", cJSON_IsArray(ws) ? cJSON_GetArraySize(ws) : 0);
    } else if (!strcmp(t, "turn")) {
        ev.type = NA_EV_TURN;
        ev.reason = parse_reason(sget(m, "reason"));
        ev.detail = sget(m, "detail");
        const cJSON *arr = cJSON_GetObjectItemCaseSensitive(m, "calls");
        ncalls = cJSON_IsArray(arr) ? (size_t)cJSON_GetArraySize(arr) : 0;
        if (ncalls) {
            calls = xcalloc(ncalls, sizeof *calls);
            owned = xcalloc(ncalls * 2, sizeof *owned);   /* 参数文本、预检的错误 */
        }
        const char *pol_s = na_core_policy(k->core, ev.sid);
        run_pol = pol_s ? cJSON_Parse(pol_s) : NULL;
        for (size_t i = 0; i < ncalls; i++) {
            const cJSON *it = cJSON_GetArrayItem(arr, (int)i);
            const cJSON *args = cJSON_GetObjectItemCaseSensitive(it, "args");
            calls[i].id = sget(it, "id");
            calls[i].name = sget(it, "name");
            if (!args) {
                calls[i].args = "{}";
                calls[i].args_ok = true;
            } else if (cJSON_IsString(args)) {
                calls[i].args = args->valuestring;   /* 网关没能解析成对象，原样带过来 */
            } else {
                owned[i] = cJSON_PrintUnformatted(args);
                calls[i].args = owned[i];
                calls[i].args_ok = cJSON_IsObject(args);
            }
            if (calls[i].args_ok && cJSON_IsObject(args))
                calls[i].elevated = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(args, "elevated"));
            /* 先找好工作目录：写错了直接告诉模型；有没有沙箱按那台机器算 */
            if (calls[i].args_ok && calls[i].name && strcmp(calls[i].name, "Skill") &&
                strcmp(calls[i].name, "Gateway") && strcmp(calls[i].name, "GatewayAdmin") && run_pol) {
                char err[1024];
                const cJSON *ws = pick_workspace(run_pol, args, err, sizeof err);
                if (!ws) {
                    owned[ncalls + i] = xstrdup(err);
                    calls[i].error = owned[ncalls + i];
                } else {
                    bool local = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(ws, "local"));
                    const cJSON *ll = cJSON_GetObjectItemCaseSensitive(ws, "landlock");
                    int landlock = local ? k->landlock : cJSON_IsNumber(ll) ? ll->valueint : 0;
                    calls[i].sandbox = landlock > 0 ? NA_SB_YES : NA_SB_NO;
                }
            }
            log_info("模型请求 %s（%s）", calls[i].name ? calls[i].name : "?", calls[i].id ? calls[i].id : "?");
        }
        ev.calls = calls;
        ev.ncalls = ncalls;
    } else if (!strcmp(t, "approve")) {
        ev.type = NA_EV_APPROVE;
        ev.id = sget(m, "id");
        ev.ok = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "ok"));
        ev.from = sget(m, "from");
    } else if (!strcmp(t, "cancel")) {
        ev.type = NA_EV_CANCEL;
    } else {
        log_debug("忽略未知消息：%s", t);
        return;
    }

    if (na_core_feed(k->core, now, &ev) < 0)
        log_warn("忽略与当前状态不符的消息：%s sid=%s", t, ev.sid ? ev.sid : "-");

    for (size_t i = 0; i < ncalls; i++) {
        cJSON_free(owned[i]);
        free(owned[ncalls + i]);
    }
    free(owned);
    free(calls);
    cJSON_free(policy);
    cJSON_Delete(run_pol);
}

/* ---- 连接管理 ---- */

static void try_connect(kernel_t *k, int64_t now)
{
    char err[256];
    if (link_connect(&k->link, k->conf.gateway, CONNECT_TIMEOUT_MS, err, sizeof err) < 0) {
        log_warn("%s，%d 秒后重试", err, k->backoff / 1000);
        k->next_connect = now_ms() + k->backoff;
        k->backoff = k->backoff * 2 > MAX_BACKOFF_MS ? MAX_BACKOFF_MS : k->backoff * 2;
        return;
    }
    log_info("已连接网关 %s", k->conf.gateway);
    k->last_ping = now;
    send_hello(k);
}

static void disconnect(kernel_t *k, int64_t now, const char *why)
{
    if (k->link.fd < 0) return;
    log_warn("与网关断开：%s", why);
    link_close(&k->link);
    na_event ev = { .type = NA_EV_RESET };
    na_core_feed(k->core, now, &ev);
    pump(k, now);   /* 只剩 KILL：终止所有正在执行的工具 */
    while (k->gwjobs) {
        gwjob_t *g = k->gwjobs;
        k->gwjobs = g->next;
        gw_free(g);
    }
    /* 退避到网关真正接受（welcome / pairing）才重置：连得上却总被拒（名字冲突之类）时不会每秒重连 */
    k->next_connect = now + k->backoff;
    k->backoff = k->backoff * 2 > MAX_BACKOFF_MS ? MAX_BACKOFF_MS : k->backoff * 2;
}

static void collect_finished(kernel_t *k, int64_t now)
{
    job_t *j;
    while ((j = runner_take_finished(&k->runner, now))) {
        if (!j->killed) {
            bool ok;
            char *res = job_result(j, &ok);
            log_info("%s（%s）%s", j->name, j->id, ok ? "完成" : "失败");
            if (j->kind == JOB_HOST) {
                send_run_result(k, j->id, ok, res);
            } else {
                na_event ev = { .type = NA_EV_TOOL_DONE, .sid = j->sid, .id = j->id, .ok = ok, .content = res };
                na_core_feed(k->core, now, &ev);
            }
            free(res);
        }
        job_free(j);
    }
}

static int64_t next_wakeup(kernel_t *k, int64_t now)
{
    int64_t wake = now + 1000;   /* 至少每秒醒一次，顺带处理退出后迟迟不关管道的进程 */
    int64_t dl = na_core_next_deadline(k->core);
    if (dl >= 0 && dl < wake) wake = dl;
    if (k->link.fd >= 0) {
        if (k->last_ping + PING_INTERVAL_MS < wake) wake = k->last_ping + PING_INTERVAL_MS;
    } else if (k->next_connect < wake) {
        wake = k->next_connect;
    }
    return wake;
}

static void loop(kernel_t *k)
{
    static struct pollfd pfds[MAX_POLL];
    static pollref_t refs[MAX_POLL];

    while (!g_stop) {
        int64_t now = now_ms();
        if (k->link.fd < 0 && now >= k->next_connect) try_connect(k, now);

        size_t np = 0;
        pfds[np++] = (struct pollfd){ .fd = g_wake[0], .events = POLLIN };
        int li = -1;
        if (k->link.fd >= 0) {
            li = (int)np;
            pfds[np++] = (struct pollfd){ .fd = k->link.fd, .events = POLLIN | (k->link.out.len ? POLLOUT : 0) };
        }
        size_t base = np;
        np += runner_pollfds(&k->runner, pfds + base, refs + base, MAX_POLL - base);

        int64_t wake = next_wakeup(k, now);
        int timeout = wake > now ? (int)(wake - now) : 0;
        if (poll(pfds, np, timeout) < 0 && errno != EINTR) {
            log_error("poll 失败：%s", strerror(errno));
            break;
        }
        now = now_ms();

        if (pfds[0].revents & POLLIN) {
            char tmp[64];
            while (read(g_wake[0], tmp, sizeof tmp) > 0) {}
        }
        runner_reap(&k->runner, now);
        runner_tick(&k->runner, now);

        if (li >= 0 && k->link.fd >= 0) {
            short re = pfds[li].revents;
            if (re & (POLLIN | POLLHUP | POLLERR)) {
                int rc = link_fill(&k->link, now);
                for (;;) {
                    bool bad;
                    cJSON *m = link_next(&k->link, &bad);
                    if (bad) { log_warn("收到无法解析的消息，已忽略"); continue; }
                    if (!m) break;
                    if (na_log_level <= NA_LOG_DEBUG) {
                        char *s = cJSON_PrintUnformatted(m);
                        log_debug("<- %.*s", (int)utf8_cut(s, strlen(s), 500), s);
                        cJSON_free(s);
                    }
                    handle_msg(k, m, now);
                    cJSON_Delete(m);
                }
                if (rc < 0) disconnect(k, now, "连接被关闭");
            }
            if (k->link.fd >= 0 && (re & POLLOUT) && link_flush(&k->link) < 0)
                disconnect(k, now, "写入失败");
        }

        if (np > base) runner_on_poll(&k->runner, pfds + base, refs + base, np - base);
        collect_finished(k, now);

        na_event tick = { .type = NA_EV_TICK };
        na_core_feed(k->core, now, &tick);
        pump(k, now);

        if (k->link.fd >= 0) {
            if (now - k->link.last_rx > DEAD_AFTER_MS) {
                disconnect(k, now, "长时间没有收到网关的消息");
            } else if (now - k->last_ping >= PING_INTERVAL_MS) {
                link_send(&k->link, msg_new("ping", NULL));
                k->last_ping = now;
            }
        }
        if (k->link.fd >= 0 && k->link.out.len && link_flush(&k->link) < 0)
            disconnect(k, now, "写入失败");
    }
}

/* ---- 启动 ---- */

static void usage(const char *argv0)
{
    fprintf(stderr,
            "用法：%s [-c kernel.conf] [-t] [-v]\n"
            "  -c 配置文件（默认 kernel.conf）\n"
            "  -t 只检查配置和执行器，然后退出\n"
            "  -v 输出调试日志（包括收到的每条协议消息）\n",
            argv0);
}

static void setup_signals(void)
{
    if (pipe(g_wake) < 0) {
        perror("pipe");
        exit(1);
    }
    set_nonblock(g_wake[0]);
    set_nonblock(g_wake[1]);
    set_cloexec(g_wake[0]);
    set_cloexec(g_wake[1]);

    struct sigaction sa;
    memset(&sa, 0, sizeof sa);
    sa.sa_handler = on_signal;
    sigemptyset(&sa.sa_mask);
    sa.sa_flags = SA_RESTART | SA_NOCLDSTOP;
    sigaction(SIGCHLD, &sa, NULL);
    sigaction(SIGINT, &sa, NULL);
    sigaction(SIGTERM, &sa, NULL);
    signal(SIGPIPE, SIG_IGN);
}

/* 运行 nano-exec --probe，看执行器能不能用、本机支持什么沙箱 */
static int probe_exec(kernel_t *k, char *err, size_t errlen)
{
    int p[2];
    if (pipe(p) < 0) {
        snprintf(err, errlen, "创建管道失败：%s", strerror(errno));
        return -1;
    }
    pid_t pid = fork();
    if (pid < 0) {
        snprintf(err, errlen, "fork 失败：%s", strerror(errno));
        close(p[0]);
        close(p[1]);
        return -1;
    }
    if (pid == 0) {
        dup2(p[1], 1);
        close(p[0]);
        close(p[1]);
        execl(k->conf.exec, k->conf.exec, "--probe", (char *)NULL);
        _exit(127);
    }
    close(p[1]);
    buf_t b = {0};
    char tmp[1024];
    int64_t deadline = now_ms() + 5000;
    for (;;) {
        struct pollfd pf = { .fd = p[0], .events = POLLIN };
        int64_t left = deadline - now_ms();
        if (left <= 0 || poll(&pf, 1, (int)left) <= 0) break;
        ssize_t n = read(p[0], tmp, sizeof tmp);
        if (n <= 0) break;
        buf_append(&b, tmp, (size_t)n);
    }
    close(p[0]);
    kill(pid, SIGKILL);
    waitpid(pid, NULL, 0);

    cJSON *r = cJSON_ParseWithLength(b.p ? b.p : "", b.len);
    buf_free(&b);
    const cJSON *ll = cJSON_GetObjectItemCaseSensitive(r, "landlock");
    if (!cJSON_IsNumber(ll)) {
        cJSON_Delete(r);
        snprintf(err, errlen, "执行器 %s 无法运行（用 exec = 指定它的路径）", k->conf.exec);
        return -1;
    }
    k->landlock = (int)ll->valuedouble;
    k->rg = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(r, "rg"));
    cJSON_Delete(r);
    k->exec_real = realpath(k->conf.exec, NULL);
    return 0;
}

int main(int argc, char **argv)
{
    const char *conf_path = "kernel.conf";
    bool check_only = false;
    int opt;
    while ((opt = getopt(argc, argv, "c:tvh")) != -1) {
        switch (opt) {
        case 'c': conf_path = optarg; break;
        case 't': check_only = true; break;
        case 'v': na_log_level = NA_LOG_DEBUG; break;
        default: usage(argv[0]); return opt == 'h' ? 0 : 2;
        }
    }

    kernel_t k;
    memset(&k, 0, sizeof k);
    k.link.fd = -1;
    k.backoff = 1000;

    char err[512];
    if (kconf_load(&k.conf, conf_path, err, sizeof err) < 0 || probe_exec(&k, err, sizeof err) < 0) {
        log_error("%s", err);
        kconf_free(&k.conf);
        return 1;
    }
    k.tools = tools_policies(&k.ntools);

    log_info("节点 %s，网关 %s", k.conf.node, k.conf.gateway);
    if (k.conf.skills)
        log_warn("kernel.conf 里的 skills 不再使用：技能改放在网关上（网关配置目录的 skills/），这一行可以删掉");
    if (k.landlock > 0)
        log_info("执行器 %s，沙箱 Landlock v%d%s", k.conf.exec, k.landlock, k.rg ? "" : "（没有 rg，Grep 用 grep 代替）");
    else
        log_warn("执行器 %s：本机不支持 Landlock，没有沙箱，自动模式下的 Shell 也需要审批", k.conf.exec);
    if (check_only) {
        free(k.exec_real);
        kconf_free(&k.conf);
        return 0;
    }

    na_core_config cc = {
        .tools = k.tools, .ntools = k.ntools,
        .sandbox = k.landlock > 0,
        .max_steps = k.conf.max_steps,
        .run_timeout_ms = k.conf.run_timeout * 1000,
        .approval_timeout_ms = k.conf.approval_timeout * 1000,
    };
    k.core = na_core_new(&cc);
    /* 执行器的输出是一行 JSON，内容转义后可能比原文长不少，留足余量 */
    k.runner.out_cap = k.conf.max_output * 6 + 4096;

    setup_signals();
    loop(&k);

    log_info("退出");
    /* 让执行器先清理它们启动的命令，最多等一会儿 */
    runner_kill_all(&k.runner);
    for (int64_t until = now_ms() + 2500; k.runner.jobs && now_ms() < until;) {
        runner_reap(&k.runner, now_ms());
        runner_tick(&k.runner, now_ms());
        bool alive = false;
        for (job_t *j = k.runner.jobs; j; j = j->next) alive |= !j->exited;
        if (!alive) break;
        poll(NULL, 0, 20);
    }
    for (job_t *j; (j = k.runner.jobs);) {
        if (!j->exited) kill(-j->pid, SIGKILL);
        k.runner.jobs = j->next;
        job_free(j);
    }
    link_close(&k.link);
    buf_free(&k.link.in);
    buf_free(&k.link.out);
    na_core_free(k.core);
    free(k.exec_real);
    kconf_free(&k.conf);
    return 0;
}
