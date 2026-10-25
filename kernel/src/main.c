/* main.c —— nano-kernel：连接网关，驱动状态机，调执行器执行工具
 *
 * 单线程 poll 循环：网关连接、执行器的管道、信号唤醒都在同一个 poll 里。
 * 外部消息 → 事件 → na_core_feed；na_core_next 取出的命令 → 发给网关或交给执行器。
 * 平台相关的东西（找自己的路径、HOME、文件、信号、poll、子进程）在 plat.h 后面。
 */
#include "cJSON.h"
#include "conf.h"
#include "ctl.h"
#include "core.h"
#include "link.h"
#include "pipe.h"
#include "plat.h"
#include "runner.h"
#include "tools.h"
#include "update.h"
#include "util.h"

#include <ctype.h>
#include <errno.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "version.h"

#define PROTO_VERSION      10
#define PING_INTERVAL_MS   30000
#define DEAD_AFTER_MS      90000    /* 这么久没收到网关任何消息就认为连接已死 */
#define CONNECT_TIMEOUT_MS 5000
#define MAX_BACKOFF_MS     30000
#define MAX_POLL           1024

static volatile int g_stop;
static na_wake g_wake = { -1, -1 };   /* 信号处理函数往这里写一个字节，唤醒 poll */

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
    na_pipes        pipes;            /* 协议 v10：MCP 服务的 stdio 透传 */
    struct gwjob   *gwjobs;      /* 交给网关执行、还没有结果的工具调用 */
    link_t          link;
    int64_t         last_ping;
    int64_t         next_connect;
    int             backoff;
    bool            restart;          /* 换上了新版本：退出主循环后重启自己 */
    char           *pid_file;         /* 配置目录下的 kernel.pid、status.json、stop：给 nano-kernel status / stop 用 */
    char           *status_file;
    char           *stop_file;
    char            state[16];        /* 写进 status.json 的连接状态 */
    int64_t         update_deadline;  /* 刚更新完：这之前要被网关接受，否则退回旧版本 */
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
    cJSON_AddStringToObject(m, "os", plat_os());
    cJSON_AddStringToObject(m, "arch", plat_arch());
    cJSON_AddStringToObject(m, "version", NA_VERSION);
    char *failed = update_failed_version();
    if (failed) {
        cJSON_AddStringToObject(m, "update_failed", failed);
        free(failed);
    }
    cJSON_AddItemToObject(m, "tools", tools_specs());
    cJSON *sb = cJSON_AddObjectToObject(m, "sandbox");
    cJSON_AddNumberToObject(sb, "landlock", k->landlock);
    link_send(&k->link, m);
}

/* 连接状态写进 status.json（nano-kernel status 读）：connecting / connected / pairing / denied / rejected */
static void set_state(kernel_t *k, const char *state, const char *detail, const char *code)
{
    if (!k->status_file) return;
    snprintf(k->state, sizeof k->state, "%s", state);
    cJSON *m = cJSON_CreateObject();
    cJSON_AddNumberToObject(m, "pid", (double)plat_getpid());
    cJSON_AddStringToObject(m, "state", state);
    if (detail) cJSON_AddStringToObject(m, "detail", detail);
    if (code) cJSON_AddStringToObject(m, "code", code);
    cJSON_AddStringToObject(m, "gateway", k->conf.gateway);
    cJSON_AddStringToObject(m, "node", k->conf.node);
    cJSON_AddStringToObject(m, "version", NA_VERSION);
    cJSON_AddNumberToObject(m, "landlock", k->landlock);
    cJSON_AddNumberToObject(m, "since", (double)time(NULL));
    char *s = cJSON_PrintUnformatted(m);
    plat_write_atomic(k->status_file, s, strlen(s), 0600);
    cJSON_free(s);
    cJSON_Delete(m);
}

/* ---- 命令 → 外部动作 ---- */

/* real（已解析符号链接）是不是包含内核自己：配置目录或执行器 */
static bool holds_self(const kernel_t *k, const char *real)
{
    const char *mine[] = { k->conf.dir, k->exec_real };
    for (size_t i = 0; i < 2; i++)
        if (mine[i] && plat_inside(mine[i], real)) return true;
    return false;
}

/* 工作目录不能包含内核自己：配置里有 token，执行器要是能被改写，沙箱就形同虚设 */
static bool guard_paths(const kernel_t *k, const cJSON *list, char *err, size_t errlen)
{
    const cJSON *it;
    cJSON_ArrayForEach(it, list) {
        if (!cJSON_IsString(it)) continue;
        const char *p = it->valuestring;
        char *full = plat_expand_home(p);
        char *real = plat_canonical(full);
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
    char *home = plat_home();
    bool roots = NA_WINDOWS && (!p || !*p || !strcmp(p, "/") || !strcmp(p, "\\"));
    char *full = roots ? xstrdup(home) : plat_expand_home(p && *p ? p : "~");
    cJSON *r = msg_new("ls_result", NULL);
    cJSON_AddStringToObject(r, "id", id ? id : "");
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
        char *abs = plat_abspath(NULL, full);
        char *real = abs ? plat_canonical(abs) : NULL;
        free(abs);
        plat_dirent *ents = NULL;
        size_t n = 0;
        bool more = false;
        char err[256] = "";
        if (!real || !plat_read_dir(real, LS_MAX, &ents, &n, &more, err, sizeof err)) {
            cJSON_AddBoolToObject(r, "ok", false);
            cJSON_AddStringToObject(r, "detail", !plat_is_abs(full) ? "要写绝对路径或 ~ 开头"
                                       : real ? err : "目录不存在或不可访问");
        } else {
            cJSON_AddBoolToObject(r, "ok", true);
            cJSON_AddStringToObject(r, "path", real);
            cJSON_AddBoolToObject(r, "blocked", holds_self(k, real));
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
                if (holds_self(k, sub)) cJSON_AddBoolToObject(it, "blocked", true);
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
    free(full);
    free(home);
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
static bool ws_is_windows(const cJSON *it)
{
    const char *os = sget(it, "os");
    return os && !strcmp(os, "windows");
}

/* 路径属于那台机器：Windows 上不区分大小写，\ 和 / 当成一样 */
static bool ws_path_eq(const char *a, const char *b, bool win)
{
    if (!a || !b) return false;
    if (!win) return !strcmp(a, b);
    while (*a && *b) {
        char ca = *a == '\\' ? '/' : *a;
        char cb = *b == '\\' ? '/' : *b;
        if (tolower((unsigned char)ca) != tolower((unsigned char)cb)) return false;
        a++;
        b++;
    }
    return !*a && !*b;
}

static bool ws_part_is(const cJSON *it, const char *want, bool machine)
{
    const char *name = sget(it, "name");
    const char *colon = name ? strchr(name, ':') : NULL;
    if (!colon) return false;
    return machine ? (size_t)(colon - name) == strlen(want) && !strncmp(name, want, (size_t)(colon - name))
                   : ws_path_eq(colon + 1, want, ws_is_windows(it));
}

/* 完整的“机器:路径” */
static bool ws_name_is(const cJSON *it, const char *want)
{
    const char *name = sget(it, "name");
    if (!name) return false;
    if (!strcmp(name, want)) return true;
    if (!ws_is_windows(it)) return false;
    const char *a = strchr(name, ':'), *b = strchr(want, ':');
    if (!a || !b) return false;
    if ((size_t)(a - name) != (size_t)(b - want) || strncmp(name, want, (size_t)(a - name))) return false;
    return ws_path_eq(a + 1, b + 1, true);
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
            if (ws_name_is(it, w)) return it;
        char *end;
        long i = strtol(w, &end, 10);
        if (!*end && i >= 1 && i <= n) return cJSON_GetArrayItem(list, (int)i - 1);
        for (int pass = 0; pass < 2 && !hits; pass++) {
            cJSON_ArrayForEach(it, list) {
                if (ws_part_is(it, w, pass == 0)) {
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
/* 这一轮工具结果的上限（网关在 input 里带上的；没带用 kernel.conf） */
static size_t run_max_output(const kernel_t *k, const cJSON *pol)
{
    const cJSON *mo = cJSON_GetObjectItemCaseSensitive(pol, "max_output");
    return cJSON_IsNumber(mo) ? (size_t)mo->valuedouble : k->conf.max_output;
}

static void send_exec(kernel_t *k, const na_cmd *c, cJSON *args, const char *workspace, size_t max_output)
{
    cJSON *m = msg_new("exec", c->sid);
    cJSON_AddStringToObject(m, "id", c->id);
    cJSON_AddStringToObject(m, "name", c->name);
    cJSON_AddItemToObject(m, "args", args ? args : cJSON_CreateString(c->args ? c->args : ""));
    if (workspace) cJSON_AddStringToObject(m, "workspace", workspace);
    if (c->elevated) cJSON_AddBoolToObject(m, "elevated", true);
    cJSON_AddNumberToObject(m, "timeout_ms", c->timeout_ms);
    cJSON_AddNumberToObject(m, "max_output", (double)max_output);
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
        const cJSON *mo = cJSON_GetObjectItemCaseSensitive(req, "max_output");
        if (!cJSON_IsNumber(mo))
            cJSON_AddNumberToObject(req, "max_output", (double)k->conf.max_output);
        else if ((size_t)mo->valuedouble * 6 + 4096 > k->runner.out_cap)
            k->runner.out_cap = (size_t)mo->valuedouble * 6 + 4096;   /* 执行器的输出是转义后的 JSON，留足余量 */
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

/* 由网关执行的工具：内核只转给网关，不挑工作目录、不交给执行器 */
static bool gateway_tool(const char *name)
{
    static const char *const names[] = { "Skill", "Gateway", "GatewayAdmin", "WebSearch", "WebFetch", "Schedule", "Message",
                                         "Knowledge", "Diary", "FollowUp", "SendFile" };
    for (size_t i = 0; i < sizeof names / sizeof names[0]; i++)
        if (!strcmp(name, names[i])) return true;
    return false;
}

static void exec_tool(kernel_t *k, const na_cmd *c, int64_t now)
{
    char err[1024] = "";
    cJSON *args = cJSON_Parse(c->args ? c->args : "{}");
    cJSON *pol = cJSON_Parse(c->policy ? c->policy : "{}");
    size_t max_output = run_max_output(k, pol);
    if (gateway_tool(c->name)) {
        send_exec(k, c, args, NULL, max_output);
        cJSON_Delete(pol);
        return;
    }
    const cJSON *ws = pick_workspace(pol, args, err, sizeof err);
    job_t *j = NULL;
    if (ws) {
        cJSON_DeleteItemFromObjectCaseSensitive(args, "workspace");
        if (!cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(ws, "local"))) {
            /* 工作目录不在这台机器上：交给网关，约束由网关按会话的设置生成 */
            send_exec(k, c, args, sget(ws, "name"), max_output);
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
        cJSON_AddNumberToObject(req, "max_output", (double)max_output);
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

static bool put_skill(const char *name, const cJSON *files, char *dir_out, size_t dirlen, char *err, size_t errlen)
{
    char *home = plat_home();
    if (!name || !*name || strspn(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != strlen(name) || name[0] == '.') {
        snprintf(err, errlen, "技能名字不能用：%s", name ? name : "");
        free(home);
        return false;
    }
    const char *nh = getenv("NANO_HOME");
    char *base = nh && plat_is_abs(nh) ? xstrdup(nh) : plat_join(home, ".nano-agent");
    char *root = plat_join(base, "skills");
    char *dir = plat_join(root, name);
    char *tmpname = xasprintf(".%s.tmp", name);
    char *tmp = plat_join(root, tmpname);
    free(tmpname);
    free(base);

    bool ok = plat_makedirs(root) == 0;
    if (ok) plat_remove_tree(tmp);
    ok = ok && plat_mkdir(tmp) == 0;
    if (!ok) snprintf(err, errlen, "建不了目录 %s", tmp);
    const cJSON *f;
    cJSON_ArrayForEach(f, files) {
        if (!ok) break;
        const char *rel = sget(f, "path"), *data = sget(f, "data");
        if (!safe_rel(rel) || !data) {
            snprintf(err, errlen, "技能文件路径不能用：%s", rel ? rel : "");
            ok = false;
            break;
        }
        size_t n = 0;
        unsigned char *bytes = b64_decode(data, &n);
        char *full = plat_join(tmp, rel);
        char *fdir = plat_dirname(full);
        bool made = bytes && plat_makedirs(fdir) == 0;
        if (made)
            made = plat_write_atomic(full, (const char *)bytes, n,
                                     cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(f, "exec")) ? 0755 : 0644) == 0;
        if (!made) snprintf(err, errlen, "写不了 %s：%s", rel, bytes ? strerror(errno) : "数据不是 base64");
        ok = made;
        free(fdir);
        free(full);
        free(bytes);
    }
    if (ok) {
        plat_remove_tree(dir);
        ok = plat_rename(tmp, dir) == 0;
        if (!ok) snprintf(err, errlen, "放不到 %s：%s", dir, strerror(errno));
    }
    if (!ok) plat_remove_tree(tmp);
    else snprintf(dir_out, dirlen, "%s", dir);
    free(root);
    free(dir);
    free(tmp);
    free(home);
    return ok;
}

static void handle_skill_put(kernel_t *k, const cJSON *m)
{
    char err[512] = "", dir[4096] = "";
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

/* ---- 协议 v10：MCP 服务的 stdio 透传 ---- */

/* 网关要把输出送出去：一行一行、以及退出 */
static void pump_pipes(kernel_t *k)
{
    na_pipe_msg m;
    while (pipes_take(&k->pipes, &m)) {
        cJSON *out;
        if (!m.data) {   /* 退出的那条没有 data（退出码可以是 -1：没有这条管道、起不来） */
            out = msg_new("pipe_closed", NULL);
            cJSON_AddStringToObject(out, "pipe", m.id);
            cJSON_AddNumberToObject(out, "exit", m.exit);
            if (m.detail && *m.detail) cJSON_AddStringToObject(out, "detail", m.detail);
        } else {
            out = msg_new("pipe_data", NULL);
            cJSON_AddStringToObject(out, "pipe", m.id);
            cJSON_AddStringToObject(out, "data", m.data ? m.data : "");
        }
        link_send(&k->link, out);
        free(m.id);
        free(m.data);
        free(m.detail);
    }
}

static void handle_pipe_open(kernel_t *k, const cJSON *m)
{
    const char *id = sget(m, "pipe");
    const cJSON *av = cJSON_GetObjectItemCaseSensitive(m, "argv");
    if (!id || !cJSON_IsArray(av)) return;
    int n = cJSON_GetArraySize(av);
    char **argv = xcalloc((size_t)n + 1, sizeof *argv);
    for (int i = 0; i < n; i++) {
        const cJSON *v = cJSON_GetArrayItem(av, i);
        argv[i] = (char *)(cJSON_IsString(v) ? v->valuestring : "");
    }
    /* env：k, v 交替，NULL 结尾 */
    const cJSON *ev = cJSON_GetObjectItemCaseSensitive(m, "env");
    size_t pairs = cJSON_IsObject(ev) ? (size_t)cJSON_GetArraySize(ev) : 0;
    const char **env = xcalloc(pairs * 2 + 2, sizeof *env);
    size_t pi = 0;
    if (cJSON_IsObject(ev)) {
        const cJSON *it = NULL;
        cJSON_ArrayForEach(it, ev) {
            if (!cJSON_IsString(it) || !it->string) continue;
            env[pi++] = it->string;
            env[pi++] = it->valuestring;
        }
    }
    char err[256] = "";
    if (pipes_open(&k->pipes, id, argv, env, sget(m, "cwd"), err, sizeof err) < 0) {
        log_warn("起 MCP 进程失败：%s", err);
        cJSON *out = msg_new("pipe_closed", NULL);
        cJSON_AddStringToObject(out, "pipe", id);
        cJSON_AddNumberToObject(out, "exit", -1);
        cJSON_AddStringToObject(out, "detail", err);
        link_send(&k->link, out);
    } else {
        log_info("起了 MCP 进程 %s：%s", id, argv[0]);
    }
    free(argv);
    free(env);
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
    /* 工具结果的上限：网关每一轮带上（没带就用 kernel.conf 的 max_output） */
    const cJSON *mo = cJSON_GetObjectItemCaseSensitive(m, "max_output");
    if (cJSON_IsNumber(mo) && mo->valuedouble >= 1024 && mo->valuedouble <= (64 << 20))
        cJSON_AddNumberToObject(p, "max_output", mo->valuedouble);
    char *s = cJSON_PrintUnformatted(p);
    cJSON_Delete(p);
    return s;
}

static void disconnect(kernel_t *k, int64_t now, const char *why);

/* 网关同意了配对：口令存进配置目录（只有自己能读），用它重新连上去 */
static void save_token(kernel_t *k, const char *token, int64_t now)
{
    if (!token || !*token || strchr(token, '\n')) return;
    char *line = xasprintf("%s\n", token);
    if (plat_write_atomic(k->conf.token_file, line, strlen(line), 0600) < 0)
        log_warn("保存配对口令失败（%s），下次启动要重新配对：%s", k->conf.token_file, strerror(errno));
    free(line);
    free(k->conf.token);
    k->conf.token = xstrdup(token);
    k->conf.paired = true;
    log_info("网关同意了这台电脑接入，口令已保存到 %s", k->conf.token_file);
    disconnect(k, now, "配对完成，重新连接");
    k->next_connect = now;
}

/* 网关推来新版本：换掉自己，回报结果，然后重启 */
static void handle_update(kernel_t *k, const cJSON *m)
{
    const char *ver = sget(m, "version");
    log_info("网关推来新版本 %s，开始更新", ver ? ver : "?");
    char err[512] = "";
    bool ok = update_apply(m, k->conf.exec, err, sizeof err) == 0;
    cJSON *r = msg_new("update_result", NULL);
    cJSON_AddBoolToObject(r, "ok", ok);
    if (ver) cJSON_AddStringToObject(r, "version", ver);
    if (!ok) {
        cJSON_AddStringToObject(r, "detail", err);
        log_error("更新失败：%s", err);
    }
    link_send(&k->link, r);
    if (ok) {
        k->restart = true;
        g_stop = 1;
    }
}

static void handle_msg(kernel_t *k, const cJSON *m, int64_t now)
{
    const char *t = sget(m, "t");
    if (!t) return;
    if (!strcmp(t, "pong")) return;
    if (!strcmp(t, "welcome")) {
        k->backoff = 1000;
        log_info("网关已接受连接");
        set_state(k, "connected", NULL, NULL);
        if (k->update_deadline) {
            update_confirm();
            k->update_deadline = 0;
            log_info("更新到 %s 成功", NA_VERSION);
        }
        return;
    }
    if (!strcmp(t, "update")) {
        handle_update(k, m);
        return;
    }
    if (!strcmp(t, "error")) {
        const char *reason = sget(m, "reason") ? sget(m, "reason") : "";
        log_error("网关报错：%s", sget(m, "detail") ? sget(m, "detail") : "（无详情）");
        if (!strcmp(reason, "denied")) {
            log_error("网关没有同意这台电脑接入，内核退出。要重新申请，再启动一次即可");
            set_state(k, "denied", NULL, NULL);
            g_stop = 1;
        } else if (!strcmp(reason, "auth") && k->conf.paired) {
            log_warn("网关不认这台电脑的配对口令了（可能在网页里移除了它），重新请求配对");
            plat_unlink(k->conf.token_file);
            k->conf.token[0] = 0;
            k->conf.paired = false;
        } else {
            set_state(k, "rejected", sget(m, "detail"), NULL);
        }
        return;
    }
    if (!strcmp(t, "pairing")) {
        k->backoff = 1000;
        log_info("等待网关确认接入：打开网关网页（会弹出接入请求），核对确认码 %s 后点「允许」",
                 sget(m, "code") ? sget(m, "code") : "?");
        set_state(k, "pairing", NULL, sget(m, "code"));
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
    if (!strcmp(t, "pipe_open")) {
        handle_pipe_open(k, m);
        return;
    }
    if (!strcmp(t, "pipe_write")) {
        if (pipes_write(&k->pipes, sget(m, "pipe"), sget(m, "data")) != 0)
            log_warn("往 MCP 进程写失败");
        return;
    }
    if (!strcmp(t, "pipe_close")) {
        pipes_close(&k->pipes, sget(m, "pipe"));
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
        /* 网关的设置：大于 0 是上限，小于 0 不限，没有（旧网关）用 kernel.conf */
        const cJSON *ms = cJSON_GetObjectItemCaseSensitive(m, "max_steps");
        const cJSON *rt = cJSON_GetObjectItemCaseSensitive(m, "run_timeout_ms");
        const cJSON *at = cJSON_GetObjectItemCaseSensitive(m, "approval_timeout_ms");
        ev.max_steps = cJSON_IsNumber(ms) ? ms->valueint : 0;
        ev.run_timeout_ms = cJSON_IsNumber(rt) ? rt->valueint : 0;
        ev.approval_timeout_ms = cJSON_IsNumber(at) ? at->valueint : 0;
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
            calls[i].action = sget(args, "action");
            /* 先找好工作目录：写错了直接告诉模型；有没有沙箱按那台机器算 */
            if (calls[i].args_ok && calls[i].name && !gateway_tool(calls[i].name) && run_pol) {
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
        set_state(k, "connecting", err, NULL);
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
    if (k->link.fd == NA_SOCK_NONE) return;
    log_warn("与网关断开：%s", why);
    /* 被拒绝、等确认这些状态比“断开”更有用，留着 */
    if (!strcmp(k->state, "connected") || !k->state[0]) set_state(k, "connecting", why, NULL);
    link_close(&k->link);
    /* 网关那边已经不在了：把它让起的 MCP 进程都收掉，别留在机器上。
       用 reset 不用 shutdown：这一轮主循环后面的 pipes_poll 还拿着这些条目的引用 */
    pipes_reset(&k->pipes);
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
    if (k->link.fd != NA_SOCK_NONE) {
        if (k->last_ping + PING_INTERVAL_MS < wake) wake = k->last_ping + PING_INTERVAL_MS;
    } else if (k->next_connect < wake) {
        wake = k->next_connect;
    }
    return wake;
}

static void loop(kernel_t *k)
{
    static na_pollfd pfds[MAX_POLL];
    static pollref_t refs[MAX_POLL];
    static na_piperef prefs[MAX_POLL];

    while (!g_stop) {
        int64_t now = now_ms();
        plat_stat_t stop_st;
        if (k->stop_file && plat_stat(k->stop_file, &stop_st) && stop_st.exists) {
            plat_unlink(k->stop_file);
            log_info("收到停止请求（nano-kernel stop）");
            break;
        }
        if (k->update_deadline && now >= k->update_deadline) {
            if (update_rollback("更新后一直没能连上网关") == 0) {
                k->restart = true;
                break;
            }
            k->update_deadline = 0;
        }
        if (k->link.fd == NA_SOCK_NONE && now >= k->next_connect) try_connect(k, now);

        size_t np = 0;
        pfds[np++] = (na_pollfd){ .id = g_wake.rd, .events = NA_POLLIN };
        int li = -1;
        if (k->link.fd != NA_SOCK_NONE) {
            li = (int)np;
            pfds[np++] = (na_pollfd){ .id = (intptr_t)k->link.fd,
                                      .events = NA_POLLIN | (k->link.out.len ? NA_POLLOUT : 0) };
        }
        size_t base = np;
        np += runner_pollfds(&k->runner, pfds + base, refs + base, MAX_POLL - base);
        size_t pbase = np;
        np += pipes_pollfds(&k->pipes, pfds + pbase, prefs + pbase, MAX_POLL - pbase);

        int64_t wake = next_wakeup(k, now);
        int timeout = wake > now ? (int)(wake - now) : 0;
        if (timeout > 1000) timeout = 1000;   /* 至少每秒看一次有没有 nano-kernel stop 的请求 */
        if (plat_poll(pfds, np, timeout) < 0 && errno != EINTR) {
            log_error("poll 失败：%s", strerror(errno));
            break;
        }
        now = now_ms();

        if (pfds[0].revents & NA_POLLIN) plat_wake_drain(&g_wake);
        runner_reap(&k->runner, now);
        runner_tick(&k->runner, now);

        if (li >= 0 && k->link.fd != NA_SOCK_NONE) {
            short re = pfds[li].revents;
            if (re & (NA_POLLIN | NA_POLLHUP | NA_POLLERR)) {
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
            if (k->link.fd != NA_SOCK_NONE && (re & NA_POLLOUT) && link_flush(&k->link) < 0)
                disconnect(k, now, "写入失败");
        }

        if (pbase > base) runner_on_poll(&k->runner, pfds + base, refs + base, pbase - base);
        pipes_poll(&k->pipes, pfds + pbase, prefs + pbase, np - pbase, now);
        pump_pipes(k);
        collect_finished(k, now);

        na_event tick = { .type = NA_EV_TICK };
        na_core_feed(k->core, now, &tick);
        pump(k, now);

        if (k->link.fd != NA_SOCK_NONE) {
            if (now - k->link.last_rx > DEAD_AFTER_MS) {
                disconnect(k, now, "长时间没有收到网关的消息");
            } else if (now - k->last_ping >= PING_INTERVAL_MS) {
                link_send(&k->link, msg_new("ping", NULL));
                k->last_ping = now;
            }
        }
        if (k->link.fd != NA_SOCK_NONE && k->link.out.len && link_flush(&k->link) < 0)
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
            "  -v 输出调试日志（包括收到的每条协议消息）\n"
            "  --version 显示版本\n",
            argv0);
}

static void setup_signals(void)
{
    if (plat_wake_open(&g_wake) < 0) {
        perror("wake");
        exit(1);
    }
    plat_signals_install(&g_wake, &g_stop);
}

/* 运行 nano-exec --probe，看执行器能不能用、本机支持什么沙箱 */
static int probe_exec(kernel_t *k, char *err, size_t errlen)
{
    char *argv[] = { k->conf.exec, "--probe", NULL };
    buf_t b = {0};
    if (plat_probe(argv, 5000, &b) < 0) {
        snprintf(err, errlen, "执行器 %s 无法运行（用 exec = 指定它的路径）", k->conf.exec);
        buf_free(&b);
        return -1;
    }
    cJSON *r = cJSON_ParseWithLength(b.p ? b.p : "", b.len);
    buf_free(&b);
    const cJSON *ll = cJSON_GetObjectItemCaseSensitive(r, "landlock");
    if (!cJSON_IsNumber(ll)) {
        cJSON_Delete(r);
        snprintf(err, errlen, "执行器 %s 的自检结果看不懂（用 exec = 指定它的路径）", k->conf.exec);
        return -1;
    }
    k->landlock = (int)ll->valuedouble;
    k->rg = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(r, "rg"));
    cJSON_Delete(r);
    k->exec_real = plat_canonical(k->conf.exec);
    if (!k->exec_real) k->exec_real = xstrdup(k->conf.exec);
    return 0;
}

int main(int argc, char **argv)
{
    plat_init();
    int nargs = 0;
    char **args = plat_args(argc, argv, &nargs);
    if (nargs > 1 && ctl_is_command(args[1])) return ctl_main(nargs, args);
    if (nargs == 1) {
        plat_stat_t st;
        if (!plat_stat("kernel.conf", &st) || !st.exists) {
            ctl_usage(stdout);
            return 0;
        }
    }
    const char *conf_path = "kernel.conf";
    bool check_only = false;
    for (int i = 1; i < nargs; i++) {
        if (!strcmp(args[i], "-c") && i + 1 < nargs) conf_path = args[++i];
        else if (!strcmp(args[i], "-t")) check_only = true;
        else if (!strcmp(args[i], "--log") && i + 1 < nargs) {
            if (plat_log_to(args[++i]) < 0) fprintf(stderr, "打不开日志文件 %s\n", args[i]);
        }
        else if (!strcmp(args[i], "-v")) na_log_level = NA_LOG_DEBUG;
        else if (!strcmp(args[i], "-h") || !strcmp(args[i], "--help")) { ctl_usage(stdout); return 0; }
        else if (!strcmp(args[i], "--version")) { printf("%s\n", NA_VERSION); return 0; }
        else { usage(nargs > 0 ? args[0] : "nano-kernel"); return 2; }
    }

    update_init();
    /* 上次更新后的新版本一直起不来：退回旧版本，重启 */
    bool update_pending = false;
    if (!check_only && update_on_start(&update_pending)) {
        update_reexec(args);
        return NA_EXIT_RESTART;
    }

    kernel_t k;
    memset(&k, 0, sizeof k);
    if (update_pending) {
        const char *ms = getenv("NA_UPDATE_CONFIRM_MS");   /* 测试用：缩短等确认的时间 */
        k.update_deadline = now_ms() + (ms && atoll(ms) > 0 ? atoll(ms) : UPDATE_CONFIRM_MS);
    }
    k.link.fd = NA_SOCK_NONE;
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
        log_warn("执行器 %s：本机没有沙箱，自动模式下的 Shell 也需要审批", k.conf.exec);
    if (check_only) {
        free(k.exec_real);
        kconf_free(&k.conf);
        return 0;
    }

    /* 跟着配置文件命名：同一个目录里可以有好几份配置、跑好几个内核 */
    k.pid_file = ctl_sidecar(conf_path, ".pid");
    k.status_file = ctl_sidecar(conf_path, ".status.json");
    k.stop_file = ctl_sidecar(conf_path, ".stop");
    plat_unlink(k.stop_file);   /* 停止请求只对当时在运行的内核有效，残留的作废 */
    {
        buf_t pb = {0};
        long other = 0;
        if (plat_read_file(k.pid_file, &pb, 64) == 0 && pb.len) other = strtol(buf_cstr(&pb), NULL, 10);
        buf_free(&pb);
        if (other > 0 && other != plat_getpid() && plat_pid_alive(other, "nano-kernel")) {
            log_error("这份配置已经有一个内核在运行（pid %ld）；要重启用 nano-kernel restart", other);
            free(k.exec_real);
            kconf_free(&k.conf);
            return 1;
        }
        char *line = xasprintf("%ld\n", plat_getpid());
        plat_write_atomic(k.pid_file, line, strlen(line), 0600);
        free(line);
    }
    set_state(&k, "connecting", NULL, NULL);

    na_core_config cc = {
        .tools = k.tools, .ntools = k.ntools,
        .sandbox = k.landlock > 0,
        .max_steps = k.conf.max_steps,
        .run_timeout_ms = k.conf.run_timeout * 1000,
        .approval_timeout_ms = k.conf.approval_timeout * 1000,
    };
    k.core = na_core_new(&cc);
    pipes_init(&k.pipes);
    pipes_set_wake(&k.pipes, &g_wake);
    /* 执行器的输出是一行 JSON，内容转义后可能比原文长不少，留足余量 */
    k.runner.out_cap = k.conf.max_output * 6 + 4096;
    k.runner.wake = &g_wake;

    setup_signals();
    loop(&k);

    log_info(k.restart ? "重启" : "退出");
    if (!k.restart) {   /* 更新后的重启（exec）还是同一个进程，pid 不变，文件留着 */
        plat_unlink(k.status_file);
        plat_unlink(k.pid_file);
    }
    plat_signals_remove();
    runner_shutdown(&k.runner, 2500);
    pipes_shutdown(&k.pipes);
    if (k.restart && k.link.fd != NA_SOCK_NONE) link_flush(&k.link);   /* 把 update_result 发出去 */
    link_close(&k.link);
    buf_free(&k.link.in);
    buf_free(&k.link.out);
    na_core_free(k.core);
    free(k.exec_real);
    kconf_free(&k.conf);
    plat_wake_close(&g_wake);
    plat_shutdown();
    free(k.pid_file);
    free(k.status_file);
    free(k.stop_file);
    if (k.restart) {
        update_reexec(args);   /* POSIX 上不返回 */
        return NA_EXIT_RESTART;
    }
    return 0;
}
