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
#include "job_manager.h"
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

#define PROTO_VERSION      11

/* 沙箱请求模式（设计文档 §5）：auto 用当前可用的，on 要求完整隔离，off 明确跳过。
 * 这是“请求”，执行器回报的 sandbox_effective 才是实际生效的程度。 */
enum { SB_AUTO = 0, SB_ON, SB_OFF };
#define PING_INTERVAL_MS   30000
#define DEAD_AFTER_MS      90000    /* 这么久没收到网关任何消息就认为连接已死 */
#define CONNECT_TIMEOUT_MS 5000
#define MAX_BACKOFF_MS     30000
#define MAX_POLL           1024

/* 沙箱模式的名字，用于协议和日志 */
static const char *sb_name(int mode)
{
    return mode == SB_OFF ? "off" : mode == SB_ON ? "on" : "auto";
}

static volatile int g_stop;
static na_wake g_wake = { -1, -1 };   /* 信号处理函数往这里写一个字节，唤醒 poll */

/* 交给网关执行的工具调用（工作目录不在这台机器上，或者 Skill），等 exec_result */
typedef struct gwjob {
    char *sid, *run_id, *id;
    int   turn_id;
    bool  started;         /* 网关已经确认这个调用真的开始执行了（协议 v11） */
    struct gwjob *next;
} gwjob_t;

/* 已经派发、在等本机执行槽的调用：槽位腾出来之后按派发顺序启动。启动之前不发
 * CALL_STARTED，所以状态机那边的执行时限还没有开始走。 */
typedef struct job_binding {
    managed_job *job;
    job_t *process;  /* retained until the terminal metadata fence is acknowledged */
    char *sid,*run_id,*id,*lease_id,*lease_spec;
    int turn_id,kind,detach_phase;
    bool wait_closed,terminal_handled,long_notice,recovered,process_drained,lease_registered,detach_requested,unknown_reported,retention_pending;
    int64_t admitted_at,started_at,next_event,next_detach,next_prune;
    char *control_request,*read_request;
    uint64_t read_id;
    struct job_binding *next;
} job_binding;

typedef struct execq {
    char  *sid, *run_id, *id, *name;
    char  *res_keys, *res_xkeys;   /* 这个请求要占的资源键 */
    char  *sandbox;                /* 启动时用的沙箱模式 */
    int    turn_id;
    int    kind;                   /* JOB_TOOL / JOB_HOST */
    char *lease_id, *lease_spec;
    bool lease_asked, lease_granted;
    job_binding *managed;
    cJSON *req;
    struct execq *next;
} execq_t;

/* 内核的活跃 run 到了上限：新来的 input 先排队（有界），等有空位再喂给状态机 */
typedef struct pendin {
    cJSON *m;
    struct pendin *next;
} pendin_t;

#define PENDIN_MAX      32     /* 最多几个 input 在排队 */
#define MAX_CALLS_WIRE  256    /* 一条 turn 消息里最多认这么多调用（网关自己会先限制成 32） */

/* 执行器任务的种类：自己这一轮的工具调用，或者网关请这台机器代为执行的（run） */
enum { JOB_TOOL = 0, JOB_HOST = 1 };

typedef struct {
    kconf           conf;
    const na_tool_policy *tools;
    size_t          ntools;
    int             landlock;    /* 执行器探测到的 Landlock 版本，0 表示没有沙箱 */
    int             sb_mode;     /* 本机请求的沙箱模式：SB_AUTO / SB_ON / SB_OFF */
    int             gw_ver;      /* 网关的协议版本（welcome 里带的）；旧网关要少等一些消息 */
    bool            rg;
    bool leasing, lease_synced;
    bool jobs_mode,detach_capable,jobs_pruning;
    job_manager *jobs;
    job_binding *job_bindings;
    char jobs_error[512];
    uint64_t job_request_seq;
    int exec_stream_version;
    char node_epoch[33];
    unsigned long long lease_seq;
    int64_t next_lease_sync;
    char           *exec_real;   /* 执行器的真实路径，它不能落在工作目录里 */
    na_core        *core;
    runner_t        runner;
    na_pipes        pipes;            /* 协议 v10：MCP 服务的 stdio 透传 */
    struct gwjob   *gwjobs;      /* 交给网关执行、还没有结果的工具调用 */
    struct execq   *execq;       /* 等本机执行槽的调用 */
    struct pendin  *pendin;      /* 活跃 run 到上限时排队的 input */
    int             nshells;     /* 正在跑的 Shell 数（受 max_parallel_shells 限制） */
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

/* 这台机器实际能达到的隔离程度：off 是明确关掉，其余看有没有可用的 Landlock */
static const char *sb_effective(const kernel_t *k)
{
    if (k->sb_mode == SB_OFF) return "none";
    return k->landlock > 0 ? "full" : "none";
}

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
    cJSON_AddStringToObject(m, "node_epoch", k->node_epoch);
    cJSON_AddStringToObject(m, "os", plat_os());
    cJSON_AddStringToObject(m, "arch", plat_arch());
    cJSON_AddStringToObject(m, "version", NA_VERSION);
    char *failed = update_failed_version();
    if (failed) {
        cJSON_AddStringToObject(m, "update_failed", failed);
        free(failed);
    }
    cJSON_AddItemToObject(m, "tools", tools_specs());
    /* 能力集合：网关按机器分别协商，缺能力时不暴露相应参数 */
    cJSON *ft = cJSON_AddObjectToObject(m, "features");
    cJSON_AddNumberToObject(ft, "call_identity", 1);
    cJSON_AddNumberToObject(ft, "machine_leases", 1);
    cJSON_AddNumberToObject(ft,"job_stream",k->exec_stream_version);
    cJSON_AddNumberToObject(ft,"job_manager",k->jobs && k->exec_stream_version>=1?1:0);
    cJSON_AddNumberToObject(ft,"lease_detach",k->jobs && k->exec_stream_version>=1?1:0);
    if(k->jobs_error[0])cJSON_AddStringToObject(m,"job_storage_error",k->jobs_error);
    cJSON_AddNumberToObject(ft, "max_parallel_shells", k->conf.max_parallel_shells);
    cJSON_AddNumberToObject(ft, "bounded_parallel", 1);
    cJSON_AddNumberToObject(ft, "max_calls_per_turn", 32);
    cJSON_AddNumberToObject(ft, "user_interjection", 1);   /* 认识 model_yield：插话后的重规划也由内核数步数 */
    cJSON_AddNumberToObject(ft, "sandbox_policy", 2);
    cJSON_AddNumberToObject(ft, "max_parallel_tools", k->conf.max_parallel_tools);
    cJSON_AddNumberToObject(ft, "max_active_runs", k->conf.max_active_runs);
    cJSON_AddNumberToObject(ft, "max_execs", k->conf.max_execs);
    cJSON *sb = cJSON_AddObjectToObject(m, "sandbox");
    cJSON_AddNumberToObject(sb, "landlock", k->landlock);
    cJSON_AddStringToObject(sb, "mode", sb_name(k->sb_mode));
    cJSON_AddStringToObject(sb, "effective", sb_effective(k));
    if (k->sb_mode != SB_OFF && k->landlock == 0)
        cJSON_AddStringToObject(sb, "reason", "这台机器没有可用的 Landlock");
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

static void gw_add(kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id)
{
    gwjob_t *g = xcalloc(1, sizeof *g);
    g->sid = xstrdup(sid ? sid : "");
    g->run_id = xstrdup(run_id ? run_id : "");
    g->turn_id = turn_id;
    g->id = xstrdup(id ? id : "");
    g->next = k->gwjobs;
    k->gwjobs = g;
}

static void gw_free(gwjob_t *g)
{
    free(g->sid);
    free(g->run_id);
    free(g->id);
    free(g);
}

/* 取出（并摘下）一个交给网关的调用；没有时返回 NULL（已经超时或取消了） */
/* 消息带回来的身份必须和派发时一致（旧网关不带，按兼容处理）。
 * 只看 sid + call_id 是不够的：模型在下一轮完全可以再用同一个 call id，
 * 而上一轮的迟到结果会被当成这一轮的结果。 */
static bool gw_same_identity(const gwjob_t *g, const char *run_id, int turn_id)
{
    if (run_id && *run_id && g->run_id && *g->run_id && strcmp(g->run_id, run_id)) return false;
    if (turn_id > 0 && g->turn_id > 0 && turn_id != g->turn_id) return false;
    return true;
}

static gwjob_t *gw_take(kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id)
{
    for (gwjob_t **p = &k->gwjobs; *p; p = &(*p)->next) {
        gwjob_t *g = *p;
        if (strcmp(g->sid, sid ? sid : "") || strcmp(g->id, id ? id : "")) continue;
        if (!gw_same_identity(g, run_id, turn_id)) continue;
        *p = g->next;
        return g;
    }
    return NULL;
}

/* 只找不摘：exec_started 用它（后面还有终态结果要配对） */
static gwjob_t *gw_find(kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id)
{
    for (gwjob_t *g = k->gwjobs; g; g = g->next) {
        if (strcmp(g->sid, sid ? sid : "") || strcmp(g->id, id ? id : "")) continue;
        if (!gw_same_identity(g, run_id, turn_id)) continue;
        return g;
    }
    return NULL;
}

static void tool_done_state(kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id,
                            bool ok, const char *content, const cJSON *state, int64_t now)
{
    char *execution = state ? cJSON_PrintUnformatted(state) : NULL;
    na_event ev = { .type = NA_EV_TOOL_DONE, .sid = sid, .run_id = run_id, .turn_id = turn_id,
                    .id = id, .ok = ok, .content = content, .execution = execution };
    na_core_feed(k->core, now, &ev);
    cJSON_free(execution);
}

static void tool_done(kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id,
                      bool ok, const char *content, int64_t now)
{
    tool_done_state(k, sid, run_id, turn_id, id, ok, content, NULL, now);
}

/* 宿主收到了，但在等执行槽或等资源：执行时限还没开始走。状态机认了之后告诉网关，
 * 界面上要显示“排队中”，不能让它看起来像已经跑起来了。 */
static void call_queued(kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id,
                        const char *name, int64_t now)
{
    na_event ev = { .type = NA_EV_CALL_QUEUED, .sid = sid, .run_id = run_id, .turn_id = turn_id, .id = id };
    if (na_core_feed(k->core, now, &ev) < 0) return;
    cJSON *out = msg_new("tool_queued", sid);
    cJSON_AddStringToObject(out, "id", id ? id : "");
    cJSON_AddStringToObject(out, "name", name ? name : "");
    cJSON_AddStringToObject(out, "run_id", run_id ? run_id : "");
    cJSON_AddNumberToObject(out, "turn_id", turn_id);
    cJSON_AddStringToObject(out, "text", "等执行槽或资源");
    link_send(&k->link, out);
}

/* 宿主确认这个调用的执行器真的起来了：执行时限从现在开始算 */
static void call_started(kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id, int64_t now)
{
    na_event ev = { .type = NA_EV_CALL_STARTED, .sid = sid, .run_id = run_id, .turn_id = turn_id, .id = id };
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

/* ---- 并发分类（设计文档 §3.2） ----
 *
 * 权限分类不能代替并发分类：PUBLIC_READ 里既有 Message 这种有副作用的操作，也有
 * WebSearch 这种真正无冲突的。这里按真实工具和已验证参数算出资源键，交给状态机
 * 排队。宁可保守也不能漏：判断不出来就退到目录级、机器级的独占，总比并行写坏
 * 文件好。模型自报“只读”不放松这里的锁。
 *
 * 键的格式是 "类别:值"，多个用逗号隔开。 */

/* 把模型给的路径变成资源键里用的绝对路径：能解析符号链接就解析（同一份文件的不同
 * 写法要落到同一个键上），解析不了（比如还没创建的文件）就用拼出来的路径。 */
static char *res_path(const char *base, const char *rel)
{
    if (!rel || !*rel) return NULL;
    char *expanded = plat_expand_home(rel);
    char *expanded_base = base ? plat_expand_home(base) : NULL;
    char *full = plat_abspath(expanded_base, expanded);
    free(expanded_base);
    free(expanded);
    char *real = plat_canonical(full);
    if (real) { free(full); return real; }
    return full;
}

/* 追加一个键；值里的逗号和换行换成空格，免得把键拆坏 */
static void res_add(buf_t *b, const char *cls, const char *value)
{
    if (!value || !*value) return;
    if (b->len) buf_puts(b, ",");
    buf_puts(b, cls);
    buf_puts(b, ":");
    for (const char *p = value; *p; p++) {
        char ch = (*p == ',' || *p == '\n') ? ' ' : *p;
        buf_append(b, &ch, 1);
    }
}

/* 算出这次调用的资源键，分成共享（shared）和独占（xkeys）两组。ws 是这一轮选中的
 * 工作目录对象（里面有 dirs），可以为 NULL。
 *
 * 关键约定：**文件操作在所在的工作目录上取共享锁，Shell 在目录上取独占锁**。
 * 两条规则合起来才挡得住“Shell 和 Write 同时改同一个文件”——只比对文件名是挡不住的，
 * 因为 Shell 的参数里根本看不出它要动哪个文件。 */
static void call_resource(const char *tool, const cJSON *args, const cJSON *ws, bool broad, buf_t *shared, buf_t *xkeys)
{
    const cJSON *dirs = cJSON_GetObjectItemCaseSensitive(ws, "dirs");
    const char *base = NULL;
    const cJSON *first = cJSON_GetArrayItem(dirs, 0);
    if (cJSON_IsString(first)) base = first->valuestring;
    const char *given = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(args, "path"));
    char *home = base ? NULL : plat_home();
    if (!base) base = home;
    char *path = res_path(base, given);

    if (!strcmp(tool, "Shell")) {
        /* 一般 Shell 没法靠字符串证明没有副作用：对涉及的每个工作目录持独占锁；
         * 提权或范围不明时扩大到机器级的文件资源。模型自报“只读”不放松这里的锁。 */
        res_add(shared, "machine", "files");
        if (broad || cJSON_GetArraySize(dirs) == 0 || cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(args, "elevated"))) {
            res_add(xkeys, "machine", "files");
        } else {
            const cJSON *it;
            cJSON_ArrayForEach(it, dirs) {
                if (!cJSON_IsString(it)) continue;
                char *d = res_path(NULL, it->valuestring);
                res_add(xkeys, "dir", d);
                free(d);
            }
        }
    } else if (!strcmp(tool, "Write") || !strcmp(tool, "Edit")) {
        /* 写：目标文件独占，所在的每个工作目录共享（挡住并行的 Shell）；
         * machine:files 共享锁让提权 Shell 和我们冲突 */
        res_add(shared, "machine", "files");
        const cJSON *it;
        cJSON_ArrayForEach(it, dirs) {
            if (!cJSON_IsString(it)) continue;
            char *d = res_path(NULL, it->valuestring);
            res_add(shared, "dir", d);
            free(d);
        }
        if (path) res_add(xkeys, "file", path);
    } else if (!strcmp(tool, "Read") || !strcmp(tool, "Grep") || !strcmp(tool, "Fetch")) {
        /* 读：目录和目标文件都取共享锁，可以和别的读、别的写并行，但不能和 Shell 重叠 */
        res_add(shared, "machine", "files");
        const cJSON *it;
        cJSON_ArrayForEach(it, dirs) {
            if (!cJSON_IsString(it)) continue;
            char *d = res_path(NULL, it->valuestring);
            res_add(shared, "dir", d);
            free(d);
        }
        if (path) res_add(shared, "file", path);
    }
    if (path && strcmp(tool, "Shell")) {
        char *parent = plat_dirname(path);
        res_add(shared, "dir", parent);
        free(parent);
    }
    free(home);
    free(path);
}

/* ---- 跨 run 的资源仲裁（宿主侧） ----
 * 状态机只在同一个 run 内排队；不同会话的 Shell 和写仍然可能撞在一起，所以宿主在
 * 启动执行器之前还要和“正在跑的任务 + 已经在排队等资源的请求”比一次。 */

static bool keys_intersect(const char *a, const char *b)
{
    if (!a || !*a || !b || !*b) return false;
    for (const char *p = a; *p;) {
        const char *e = strchr(p, ',');
        size_t n = e ? (size_t)(e - p) : strlen(p);
        if (n) {
            for (const char *q = b; *q;) {
                const char *f = strchr(q, ',');
                size_t len = f ? (size_t)(f - q) : strlen(q);
                if (na_resource_overlap(p, n, q, len)) return true;
#if NA_WINDOWS
                if (n > 5 && len > 5 && ((!memcmp(p, "dir:", 4) && !memcmp(q, "dir:", 4)) ||
                                        (!memcmp(p, "file:", 5) && !memcmp(q, "file:", 5)))) {
                    bool directory = !memcmp(p, "dir:", 4);
                    size_t prefix = directory ? 4 : 5;
                    char *left = xasprintf("%.*s", (int)(n - prefix), p + prefix);
                    char *right = xasprintf("%.*s", (int)(len - prefix), q + prefix);
                    bool same = directory ? plat_inside(left, right) || plat_inside(right, left) : plat_path_eq(left, right);
                    free(left); free(right);
                    if (same) return true;
                }
#endif
                q += len + (f ? 1 : 0);
            }
        }
        p += n + (e ? 1 : 0);
    }
    return false;
}

/* 两边的资源键有没有冲突：任何一方的独占键和对方的任何键相交即冲突 */
static bool res_conflict(const char *as, const char *ax, const char *bs, const char *bx)
{
    if (keys_intersect(ax, bx) || keys_intersect(ax, bs)) return true;
    return keys_intersect(bx, as);
}

/* 这台机器上正在跑的任务有没有占着这些资源 */
static bool res_busy(const kernel_t *k, const char *shared, const char *xkeys)
{
    for (const job_t *j = k->runner.jobs; j; j = j->next)
        if (res_conflict(j->res_keys, j->res_xkeys, shared, xkeys)) return true;
    for(const job_binding *b=k->job_bindings;b;b=b->next) {
        if(b->terminal_handled || (!b->recovered && !b->process_drained))continue;
        const cJSON *request=jobs_request(b->job);
        const cJSON *spec=cJSON_GetObjectItemCaseSensitive(request,"lease");
        if(res_conflict(sget(spec,"res_keys"),sget(spec,"res_xkeys"),shared,xkeys))return true;
    }
    return false;
}

/* 交给网关：Skill（技能在网关上），或者工作目录不在这台机器上的工具调用 */
/* 这一轮工具结果的上限（网关在 input 里带上的；没带用 kernel.conf） */
static size_t run_max_output(const kernel_t *k, const cJSON *pol)
{
    const cJSON *mo = cJSON_GetObjectItemCaseSensitive(pol, "max_output");
    return cJSON_IsNumber(mo) ? (size_t)mo->valuedouble : k->conf.max_output;
}

static void send_exec(kernel_t *k, const na_cmd *c, cJSON *args, const char *workspace, size_t max_output,
                      const char *sandbox, int64_t now)
{
    cJSON *m = msg_new("exec", c->sid);
    cJSON_AddStringToObject(m, "id", c->id);
    cJSON_AddStringToObject(m, "name", c->name);
    cJSON_AddItemToObject(m, "args", args ? args : cJSON_CreateString(c->args ? c->args : ""));
    if (workspace) cJSON_AddStringToObject(m, "workspace", workspace);
    if (c->elevated) cJSON_AddBoolToObject(m, "elevated", true);
    cJSON_AddStringToObject(m, "run_id", c->run_id ? c->run_id : "");
    cJSON_AddNumberToObject(m, "turn_id", c->turn_id);
    cJSON_AddNumberToObject(m, "timeout_ms", c->timeout_ms);
    cJSON_AddNumberToObject(m, "max_output", (double)max_output);
    /* 让网关知道这一轮要的沙箱模式：它在别的机器上执行时要转给那台机器。
     * 字段名必须叫 sandbox_mode —— 顶层 sandbox 是 hello 里的能力对象（结构化），
     * 这里写字符串会让网关反序列化失败、直接断开连接。 */
    if (sandbox && *sandbox) cJSON_AddStringToObject(m, "sandbox_mode", sandbox);
    link_send(&k->link, m);
    gw_add(k, c->sid, c->run_id, c->turn_id, c->id);
    /* 旧网关不会回 exec_started：派发就算开始，行为和以前一样。新网关先回一条
     * exec_started，执行时限从那之后才走。 */
    if (k->gw_ver < 11) call_started(k, c->sid, c->run_id, c->turn_id, c->id, now);
    log_info("交给网关执行 %s（%s）%s%s", c->name, c->id, workspace ? "：" : "", workspace ? workspace : "");
}

/* 这一轮最终用在执行器上的沙箱模式：节点自己的 on / off 是部署要求，不能被网关绕开；
 * 节点是 auto 时听这一轮请求的（默认 auto）。 */
static const char *effective_sandbox(const kernel_t *k, const char *requested)
{
    if (k->sb_mode == SB_ON) return "on";
    if (k->sb_mode == SB_OFF) return "off";
    if (requested && (!strcmp(requested, "on") || !strcmp(requested, "off"))) return requested;
    return "auto";
}

/* 启动执行器。req 是请求（接管所有权），补上执行器需要的通用字段。
 * 调用方必须先确认本机还有执行槽（见 exec_admit）。 */
static job_t *spawn_exec(kernel_t *k, cJSON *req, const char *sid, const char *run_id, int turn_id,
                         const char *id, const char *name, int kind, const char *sandbox,
                         const char *res_keys, const char *res_xkeys, job_binding *managed, char *err, size_t errlen)
{
    const char *final_sb = effective_sandbox(k, sandbox);
    bool ok = !(sandbox && !strcmp(sandbox, "on") && k->sb_mode == SB_OFF);
    if (!ok) snprintf(err, errlen, "请求 sandbox=on 与执行节点的 sandbox=off 部署要求冲突，没有执行");
    /* 只检查工作目录：ro 只读放行；rw 只有提权时才会出现 /，已经过用户审批 */
    for (const char *const *key = (const char *const[]){ "dirs", NULL }; *key && ok && strcmp(final_sb, "off"); key++)
        ok = guard_paths(k, cJSON_GetObjectItemCaseSensitive(req, *key), err, errlen);
    job_t *j = NULL;
    if (ok) {
        const cJSON *mo = cJSON_GetObjectItemCaseSensitive(req, "max_output");
        size_t max_output=cJSON_IsNumber(mo) && mo->valuedouble>=1024 && mo->valuedouble<=(64u<<20)
                          ? (size_t)mo->valuedouble : k->conf.max_output;
        cJSON_DeleteItemFromObjectCaseSensitive(req,"max_output");
        cJSON_AddNumberToObject(req,"max_output",(double)max_output);
        size_t out_cap=max_output*6+4096; /* JSON escaping, bounded per invocation */
        cJSON_DeleteItemFromObjectCaseSensitive(req, "watch_stdin");
        cJSON_AddBoolToObject(req, "watch_stdin", true);
        /* 这一轮最终的沙箱请求模式：节点自己的 on/off 优先，其余听这一轮的请求。
         * 代执行请求也一样要经过这里，不能把协调节点传来的值当成已经满足本机要求。 */
        cJSON_DeleteItemFromObjectCaseSensitive(req, "sandbox");
        cJSON_AddStringToObject(req, "sandbox", final_sb);
        char *body = cJSON_PrintUnformatted(req);
        char *line = xasprintf("%s\n", body);
        cJSON_free(body);
        char *argv[] = { k->conf.exec, NULL };
        const cJSON *stream=cJSON_GetObjectItemCaseSensitive(req,"stream");
        if(cJSON_IsNumber(stream) && stream->valueint==1) {
            const char *job_id=sget(req,"job_id");
            if(k->exec_stream_version<1 || !job_id || !*job_id || strlen(job_id)>128 || strcmp(name,"Shell")) {
                snprintf(err,errlen,"目标执行器不支持所需流式协议，或 Job 身份无效，未执行");
                free(line);
            } else j=runner_start_streamed(&k->runner,argv,sid,id,name,line,strlen(line),job_id,managed?jobs_observe:NULL,managed?managed->job:NULL,err,errlen);
        } else j = runner_start_bounded(&k->runner, argv, sid, id, name, line, strlen(line), out_cap, err, errlen);
        if (j) {
            j->managed=managed;
            j->detached=managed && jobs_background(managed->job);
            if(managed)managed->process=j;
            j->run_id = xstrdup(run_id ? run_id : "");
            j->turn_id = turn_id;
            j->kind = kind;
            j->res_keys = xstrdup(res_keys ? res_keys : "");
            j->res_xkeys = xstrdup(res_xkeys ? res_xkeys : "");
            j->sandbox = xstrdup(final_sb);
            j->sandbox_requested = xstrdup(sandbox ? sandbox : "auto");
        }
    }
    cJSON_Delete(req);
    return j;
}

/* 代执行的结果要等启动之后才发，声明放前面 */
static void send_run_result(kernel_t *k, const char *id, bool ok, const char *content);

static void pump_jobs(kernel_t *k,int64_t now);
static void managed_detach(kernel_t *k,job_binding *binding,int64_t now);
static void job_control(kernel_t *k,const cJSON *message,int64_t now);
static void job_detach_result(kernel_t *k,const cJSON *message,int64_t now);
static void sync_jobs(kernel_t *k);
static cJSON *job_public(job_binding *binding);
static void managed_close_wait(kernel_t *k,job_binding *binding,bool detached,int64_t now);
static void managed_state(kernel_t *k,job_binding *binding,const char *kind,const char *error);
/* ---- 本机执行槽（设计文档 §3.1） ----
 *
 * 机器级的真实槽位在这里管：同时跑几个执行器（max_execs）、其中几个 Shell
 * （max_parallel_shells）、还没结束的任务总数（max_live_jobs）。没有槽位就先排队，
 * 排到之后再启动——排队期间状态机那边还是 DISPATCHED，执行时限没有开始走。 */

static bool is_shell_tool(const char *name)
{
    return name && !strcmp(name, "Shell");
}

/* 网关请这台机器代跑的（JOB_HOST）也算真实执行器；它没有调用身份，sid 是空的 */
static int exec_slots_used(const kernel_t *k)
{
    int used=(int)runner_count(&k->runner);
    for(const job_binding *b=k->job_bindings;b;b=b->next) {
        if(b->process && b->process->exited) {
            bool in_runner=false;
            for(const job_t *process=k->runner.jobs;process;process=process->next)if(process==b->process)in_runner=true;
            if(!in_runner)used++;
        }
        if(b->recovered && !b->terminal_handled)used++;
    }
    return used;
}

static void lease_send(kernel_t *k, const char *type, const char *id)
{
    if (!k->leasing || !id) return;
    cJSON *m = msg_new(type, NULL);
    cJSON_AddStringToObject(m, "lease_id", id);
    cJSON_AddStringToObject(m, "node_epoch", k->node_epoch);
    link_send(&k->link, m);
}

static cJSON *lease_spec(kernel_t *k, const execq_t *q)
{
    cJSON *spec = cJSON_CreateObject();
    const cJSON *origin = cJSON_GetObjectItemCaseSensitive(q->req, "origin");
    cJSON_AddStringToObject(spec, "lease_id", q->lease_id);
    const char *sid = sget(origin, "sid");
    cJSON_AddStringToObject(spec, "sid", sid && *sid ? sid : q->sid && *q->sid ? q->sid : "node-host");
    cJSON_AddStringToObject(spec, "run_id", sget(origin, "run_id") ? sget(origin, "run_id") : q->run_id);
    const cJSON *turn = cJSON_GetObjectItemCaseSensitive(origin, "turn_id");
    cJSON_AddNumberToObject(spec, "turn_id", cJSON_IsNumber(turn) ? turn->valueint : q->turn_id);
    cJSON_AddStringToObject(spec, "call_id", sget(origin, "call_id") ? sget(origin, "call_id") : q->id);
    if (sget(origin, "agent")) cJSON_AddStringToObject(spec, "agent", sget(origin, "agent"));
    const cJSON *priority = cJSON_GetObjectItemCaseSensitive(origin, "priority");
    if (cJSON_IsNumber(priority)) cJSON_AddNumberToObject(spec, "priority", priority->valueint);
    cJSON_AddStringToObject(spec, "name", q->name);
    cJSON_AddStringToObject(spec, "res_keys", q->res_keys);
    cJSON_AddStringToObject(spec, "res_xkeys", q->res_xkeys);
    cJSON_AddStringToObject(spec, "state", "queued");
    if (q->kind == JOB_HOST) cJSON_AddStringToObject(spec, "host_id", q->id);
    if(q->managed) {
        cJSON_AddStringToObject(spec,"job_id",jobs_id(q->managed->job));
        cJSON_AddBoolToObject(spec,"background",jobs_background(q->managed->job));
    }
    (void)k;
    return spec;
}

static void lease_ask(kernel_t *k, execq_t *q)
{
    if (!k->leasing || !k->lease_synced || q->lease_asked || (q->managed && !jobs_created(q->managed->job))) return;
    cJSON *m = msg_new("lease_acquire", q->sid);
    cJSON_AddStringToObject(m, "id", q->id);
    cJSON_AddStringToObject(m, "run_id", q->run_id);
    cJSON_AddNumberToObject(m, "turn_id", q->turn_id);
    cJSON_AddStringToObject(m, "lease_id", q->lease_id);
    cJSON_AddStringToObject(m, "node_epoch", k->node_epoch);
    cJSON_AddItemToObject(m, "lease", cJSON_Parse(q->lease_spec));
    q->lease_asked = true;
    link_send(&k->link, m);
}

static void lease_sync(kernel_t *k, int64_t now)
{
    cJSON *m = msg_new("lease_sync", NULL);
    cJSON_AddStringToObject(m, "node_epoch", k->node_epoch);
    cJSON *items = cJSON_AddArrayToObject(m, "leases");
    cJSON *receipts=cJSON_AddArrayToObject(m,"lease_receipts");
    for (job_t *j = k->runner.jobs; j; j = j->next) {
        if (!j->lease_id) {
            j->lease_id = xasprintf("l%s-%llu", k->node_epoch, ++k->lease_seq);
            execq_t q = { .lease_id=j->lease_id, .sid=j->sid, .run_id=j->run_id, .turn_id=j->turn_id,
                          .id=j->id, .name=j->name, .res_keys=j->res_keys, .res_xkeys=j->res_xkeys, .kind=j->kind };
            cJSON *spec = lease_spec(k, &q);
            j->lease_spec = cJSON_PrintUnformatted(spec);
            cJSON_Delete(spec);
        }
        cJSON *item = cJSON_Parse(j->lease_spec);
        cJSON_DeleteItemFromObjectCaseSensitive(item, "state");
        cJSON_AddStringToObject(item, "state", j->killed || j->exited ? "cancelling" : "running");
        if(j->managed) {
            job_binding *b=j->managed;
            cJSON_DeleteItemFromObjectCaseSensitive(item,"background");
            cJSON_AddBoolToObject(item,"background",jobs_background(b->job));
            cJSON_AddStringToObject(item,"job_id",jobs_id(b->job));
            if(b->detach_phase==2 || b->detach_phase==3)cJSON_AddStringToObject(item,"detach_request",jobs_id(b->job));
        }
        cJSON_AddItemToArray(items, item);
    }
    for (execq_t *q = k->execq; q; q = q->next) {
        if (!q->lease_id) continue;
        if(q->managed && !jobs_created(q->managed->job))continue;
        cJSON *item=cJSON_Parse(q->lease_spec);
        if(q->managed) {
            cJSON_DeleteItemFromObjectCaseSensitive(item,"background");
            cJSON_AddBoolToObject(item,"background",jobs_background(q->managed->job));
            if(q->managed->detach_phase==2 || q->managed->detach_phase==3)
                cJSON_AddStringToObject(item,"detach_request",jobs_id(q->managed->job));
        }
        cJSON_AddItemToArray(items,item);
    }
    for(job_binding *b=k->job_bindings;b;b=b->next) {
        if(!b->lease_spec || !b->recovered || b->terminal_handled)continue;
        cJSON *item=cJSON_Parse(b->lease_spec);
        cJSON_DeleteItemFromObjectCaseSensitive(item,"state");
        cJSON_AddStringToObject(item,"state","unknown");
        cJSON_DeleteItemFromObjectCaseSensitive(item,"background");
        cJSON_AddBoolToObject(item,"background",jobs_background(b->job));
        cJSON_AddItemToArray(items,item);
    }
    for(job_binding *b=k->job_bindings;b;b=b->next) {
        if(!b->lease_spec || !jobs_finished(b->job))continue;
        cJSON *snapshot=jobs_snapshot(b->job);
        const char *status=sget(snapshot,"status");
        if(status && strcmp(status,"unknown")) {
            cJSON *receipt=cJSON_Parse(b->lease_spec);
            cJSON_DeleteItemFromObjectCaseSensitive(receipt,"state");
            cJSON_AddStringToObject(receipt,"state",status);
            cJSON_AddBoolToObject(receipt,"terminal_receipt",true);
            cJSON_DeleteItemFromObjectCaseSensitive(receipt,"background");
            cJSON_AddBoolToObject(receipt,"background",jobs_background(b->job));
            cJSON_AddItemToArray(receipts,receipt);
        }
        cJSON_Delete(snapshot);
    }
    if(k->jobs_error[0])cJSON_AddStringToObject(m,"job_storage_error",k->jobs_error);
    link_send(&k->link, m);
    k->next_lease_sync = now + 5000;
}

static void execq_free(execq_t *q)
{
    if (!q) return;
    free(q->sid); free(q->run_id); free(q->id); free(q->name);
    free(q->res_keys); free(q->res_xkeys); free(q->sandbox);
    free(q->lease_id); free(q->lease_spec);
    cJSON_Delete(q->req);
    free(q);
}

static int execq_len(const kernel_t *k)
{
    int n = 0;
    for (const execq_t *q = k->execq; q; q = q->next) n++;
    return n;
}

/* 机器上还没结束的任务总数：正在跑的 + 在排队的 */
static int exec_live(const kernel_t *k)
{
    return exec_slots_used(k) + execq_len(k);
}

/* 取消一个还没启动的执行（状态机超时、用户停止） */
static bool execq_drop(kernel_t *k, const char *sid, const char *id)
{
    for (execq_t **pp = &k->execq; *pp; pp = &(*pp)->next) {
        execq_t *q = *pp;
        if (strcmp(q->sid, sid ? sid : "") || strcmp(q->id, id ? id : "")) continue;
        if(q->managed && jobs_background(q->managed->job))return true;
        if(q->managed) {
            q->managed->wait_closed=true;
            jobs_fail(q->managed->job,"cancelled","原前台等待已停止，未启动命令");
        }
        *pp = q->next;
        if(!q->managed)lease_send(k, "lease_release", q->lease_id);
        execq_free(q);
        return true;
    }
    return false;
}

static void execq_clear(kernel_t *k)
{
    execq_t **pp=&k->execq;
    while(*pp) {
        execq_t *q=*pp;
        if(q->managed && jobs_background(q->managed->job)){pp=&q->next;continue;}
        *pp=q->next;
        if(q->managed) {
            q->managed->wait_closed=true;
            jobs_fail(q->managed->job,"cancelled","连接断开，前台排队已取消，未启动命令");
        } else lease_send(k,"lease_release",q->lease_id);
        execq_free(q);
    }
}

/* 有了空槽就从队头挑第一个资源也拿得到的启动起来（不能插队：总是从头扫） */
static void pump_execq(kernel_t *k, int64_t now)
{
    if (k->leasing && !k->lease_synced) return;
    for (;;) {
        if (exec_slots_used(k) >= k->conf.max_execs) return;
        execq_t *pick = NULL;
        for (execq_t **pp = &k->execq; *pp; pp = &(*pp)->next) {
            execq_t *q = *pp;
            if(q->managed && (!jobs_created(q->managed->job) || jobs_finished(q->managed->job) ||
                ((q->managed->wait_closed && !jobs_background(q->managed->job)))))continue;
            /* Ownership publication wins over starting a background request. */
            if(q->managed && q->managed->detach_phase)continue;
            if (k->leasing && !q->lease_granted) { lease_ask(k, q); continue; }
            if (is_shell_tool(q->name) && k->nshells >= k->conf.max_parallel_shells) continue;
            if (res_busy(k, q->res_keys, q->res_xkeys)) continue;
            if(q->managed && !jobs_spawn_ready(q->managed->job)) {
                jobs_prepare_start(q->managed->job);continue;
            }
            *pp = q->next;
            pick = q;
            break;
        }
        if (!pick) return;
        char err[1024] = "";
        job_t *j = spawn_exec(k, pick->req, pick->sid, pick->run_id, pick->turn_id, pick->id, pick->name,
                              pick->kind, pick->sandbox, pick->res_keys, pick->res_xkeys, pick->managed, err, sizeof err);
        pick->req = NULL;   /* spawn_exec 接管了（成功失败都由它释放） */
        if (j) {
            j->lease_id = pick->lease_id ? xstrdup(pick->lease_id) : NULL;
            j->lease_spec = pick->lease_spec ? xstrdup(pick->lease_spec) : NULL;
            if(pick->managed)jobs_did_spawn(pick->managed->job);
            else lease_send(k, "lease_started", j->lease_id);
            j->tag = is_shell_tool(pick->name) ? 1 : 0;
            if (j->tag) k->nshells++;
            log_info("执行 %s（%s）", pick->name, pick->id);
            if(pick->managed) { /* Real started frame will close the admission timer. */ }
            else if (pick->kind == JOB_TOOL) call_started(k, pick->sid, pick->run_id, pick->turn_id, pick->id, now);
            else { cJSON *m=msg_new("run_started",NULL); cJSON_AddStringToObject(m,"id",pick->id); link_send(&k->link,m); }
        } else {
            if(pick->managed)jobs_fail(pick->managed->job,"failed",err);
            else lease_send(k, "lease_release", pick->lease_id);
            log_warn("无法执行 %s：%s", pick->name, err);
            if(pick->managed) { /* Durable terminal event owns the original result. */ }
            else if (pick->kind == JOB_TOOL) tool_done(k, pick->sid, pick->run_id, pick->turn_id, pick->id, false, err, now);
            else send_run_result(k, pick->id, false, err);
        }
        execq_free(pick);
    }
}

/* 把一次本地执行交给执行槽：有空槽、资源也拿得到就立刻起，否则排队（先来后到）。
 * 代执行请求（JOB_HOST）走同一个入口，槽位和资源规则完全一样。 */
/* 这个调用身份是不是已经在跑或者在排队：宿主的第二道防重复检查。
 * 状态机已经保证不重复派发，这里再挡一次，避免协议异常导致同一次调用执行多遍。 */
static bool exec_identity_busy(const kernel_t *k, const char *sid, const char *run_id, int turn_id, const char *id)
{
    if (!id || !*id) return false;
    for (const execq_t *q = k->execq; q; q = q->next) {
        if (q->kind != JOB_TOOL) continue;
        if (strcmp(q->sid, sid ? sid : "") || strcmp(q->id, id)) continue;
        if (q->turn_id == turn_id && (!run_id || !*run_id || !q->run_id || !*q->run_id || !strcmp(q->run_id, run_id)))
            return true;
    }
    for (const job_t *j = k->runner.jobs; j; j = j->next) {
        if (j->kind != JOB_TOOL) continue;
        if (strcmp(j->sid, sid ? sid : "") || strcmp(j->id, id)) continue;
        if (j->turn_id == turn_id && (!run_id || !*run_id || !j->run_id || !*j->run_id || !strcmp(j->run_id, run_id)))
            return true;
    }
    return false;
}

static bool exec_admit(kernel_t *k, cJSON *req, const char *sid, const char *run_id, int turn_id,
                       const char *id, const char *name, int kind, const char *sandbox,
                       const char *res_keys, const char *res_xkeys, int64_t now, char *err, size_t errlen)
{
    bool shell = is_shell_tool(name);
    if (kind == JOB_TOOL && exec_identity_busy(k, sid, run_id, turn_id, id)) {
        /* 已经在跑或在排队：不再启动第二个进程，结果由原来那一次给出 */
        log_warn("重复的派发请求，忽略：%s（%s）", name, id);
        cJSON_Delete(req);
        return true;
    }
    bool has_slot = exec_slots_used(k) < k->conf.max_execs &&
                    (!shell || k->nshells < k->conf.max_parallel_shells);
    const cJSON *job_args=cJSON_GetObjectItemCaseSensitive(req,"args");
    const char *execution=sget(job_args,"execution");
    if(shell && execution && strcmp(execution,"foreground") && !k->jobs_mode) {
        snprintf(err,errlen,"目标节点尚未协商后台 Job 能力，未执行");cJSON_Delete(req);return false;
    }
    bool managed=shell && k->jobs_mode;
    if (!managed && !k->leasing && has_slot && !k->execq && !res_busy(k, res_keys, res_xkeys)) {
        job_t *j = spawn_exec(k, req, sid, run_id, turn_id, id, name, kind, sandbox, res_keys, res_xkeys,
                              NULL, err, errlen);
        if (!j) return false;
        j->tag = shell ? 1 : 0;
        if (shell) k->nshells++;
        log_info("执行 %s（%s）", name, id);
        if (kind == JOB_TOOL) call_started(k, sid, run_id, turn_id, id, now);
        else { cJSON *m=msg_new("run_started",NULL); cJSON_AddStringToObject(m,"id",id); link_send(&k->link,m); }
        return true;
    }
    if (exec_live(k) >= k->conf.max_live_jobs) {
        snprintf(err, errlen, "这台机器上还没结束的任务已经到上限（%d 个），这个调用没有启动。请等前面的任务结束",
                 k->conf.max_live_jobs);
        cJSON_Delete(req);
        return false;
    }
    execq_t *q = xcalloc(1, sizeof *q);
    q->sid = xstrdup(sid ? sid : "");
    q->run_id = xstrdup(run_id ? run_id : "");
    q->turn_id = turn_id;
    q->id = xstrdup(id ? id : "");
    q->name = xstrdup(name ? name : "");
    q->kind = kind;
    q->sandbox = xstrdup(sandbox && *sandbox ? sandbox : "");
    q->res_keys = xstrdup(res_keys ? res_keys : "");
    q->res_xkeys = xstrdup(res_xkeys ? res_xkeys : "");
    q->req = req;
    if (k->leasing) {
        q->lease_id = xasprintf("l%s-%llu", k->node_epoch, ++k->lease_seq);
        cJSON *spec = lease_spec(k, q);
        q->lease_spec = cJSON_PrintUnformatted(spec);
        cJSON_Delete(spec);
    }
    if(managed) {
        cJSON_DeleteItemFromObjectCaseSensitive(req,"sandbox");
        cJSON_AddStringToObject(req,"sandbox",effective_sandbox(k,sandbox));
        cJSON_DeleteItemFromObjectCaseSensitive(req,"wait_id");
        cJSON_AddStringToObject(req,"wait_id",q->id);
        cJSON_DeleteItemFromObjectCaseSensitive(req,"wait_sid");
        cJSON_AddStringToObject(req,"wait_sid",q->sid);
        cJSON_DeleteItemFromObjectCaseSensitive(req,"lease");
        cJSON_AddItemToObject(req,"lease",cJSON_Parse(q->lease_spec));
        managed_job *owned=jobs_create(k->jobs,cJSON_GetObjectItemCaseSensitive(req,"origin"),req,
                                      q->res_keys,q->res_xkeys,kind,err,errlen);
        if(!owned){execq_free(q);return false;}
        job_binding *b=xcalloc(1,sizeof *b);
        b->job=owned;b->sid=xstrdup(q->sid);b->run_id=xstrdup(q->run_id);
        b->turn_id=q->turn_id;b->id=xstrdup(q->id);b->kind=kind;b->admitted_at=now;
        b->lease_id=xstrdup(q->lease_id);
        q->managed=b;
        cJSON *spec=lease_spec(k,q);
        free(q->lease_spec);q->lease_spec=cJSON_PrintUnformatted(spec);cJSON_Delete(spec);
        b->lease_spec=xstrdup(q->lease_spec);
        cJSON_Delete(q->req);q->req=cJSON_Duplicate(jobs_request(owned),true);
        b->next=k->job_bindings;k->job_bindings=b;
    }
    execq_t **pp = &k->execq;
    while (*pp) pp = &(*pp)->next;
    *pp = q;
    /* 状态机那边从“派发等待”转成“排队”：执行时限还没开始走 */
    if (kind == JOB_TOOL) call_queued(k, sid, run_id, turn_id, id, name, now);
    log_info("%s（%s）在等执行槽或资源", name, id);
    if (k->leasing) lease_ask(k, q);
    return true;
}

/* 由网关执行的工具：内核只转给网关，不挑工作目录、不交给执行器 */
static bool gateway_tool(const char *name)
{
    static const char *const names[] = { "Skill", "Gateway", "GatewayAdmin", "WebSearch", "WebFetch", "Schedule", "Message",
                                         "Knowledge", "Diary", "FollowUp", "SendFile", "Agenda", "Idea", "Mind", "Session" };
    for (size_t i = 0; i < sizeof names / sizeof names[0]; i++)
        if (!strcmp(name, names[i])) return true;
    return false;
}

/* MCP 工具名是 <服务>__<工具>，内核不认识，也不能按工作目录挑执行器；直接转给网关。
 * 以前只认上面写死的网关工具，MCP 工具在挂了多个工作目录的会话里会先报“用 workspace 参数”。*/
static bool mcp_tool(const char *name)
{
    return name && strstr(name, "__") != NULL;
}

static void exec_tool(kernel_t *k, const na_cmd *c, int64_t now)
{
    char err[1024] = "";
    cJSON *args = cJSON_Parse(c->args ? c->args : "{}");
    cJSON *pol = cJSON_Parse(c->policy ? c->policy : "{}");
    size_t max_output = run_max_output(k, pol);
    if (gateway_tool(c->name) || mcp_tool(c->name)) {
        const char *sbx = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(pol, "sandbox"));
        send_exec(k, c, args, NULL, max_output, sbx, now);
        cJSON_Delete(pol);
        return;
    }
    const cJSON *ws = pick_workspace(pol, args, err, sizeof err);
    bool admitted = false;
    /* 资源键和这一轮的沙箱模式：参数还在手上时先算好（后面 args 会被搬进请求对象） */
    buf_t shared_keys = {0}, xkeys = {0};
    const char *sandbox = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(pol, "sandbox"));
    if (ws && sget(ws, "sandbox_mode")) sandbox = sget(ws, "sandbox_mode");
    if (ws) {
        const char *effective = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(ws, "local")) ? effective_sandbox(k, sandbox) : sandbox;
        call_resource(c->name, args, ws, effective && !strcmp(effective, "off"), &shared_keys, &xkeys);
    }
    if (ws) {
        cJSON_DeleteItemFromObjectCaseSensitive(args, "workspace");
        if (!cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(ws, "local"))) {
            /* 工作目录不在这台机器上：交给网关，约束由网关按会话的设置生成 */
            send_exec(k, c, args, sget(ws, "name"), max_output, sandbox, now);
            buf_free(&shared_keys);
            buf_free(&xkeys);
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
        cJSON *origin = cJSON_AddObjectToObject(req, "origin");
        cJSON_AddStringToObject(origin, "sid", c->sid);
        cJSON_AddStringToObject(origin, "run_id", c->run_id ? c->run_id : "");
        cJSON_AddNumberToObject(origin, "turn_id", c->turn_id);
        cJSON_AddStringToObject(origin, "call_id", c->id);
        if (sget(pol, "owner_agent")) cJSON_AddStringToObject(origin, "agent", sget(pol, "owner_agent"));
        if(c->from)cJSON_AddStringToObject(origin,"created_by",c->from);
        const cJSON *priority = cJSON_GetObjectItemCaseSensitive(pol, "priority");
        cJSON_AddNumberToObject(origin, "priority", cJSON_IsNumber(priority) ? priority->valueint : 0);
        cJSON_AddNumberToObject(req, "max_output", (double)max_output);
        admitted = exec_admit(k, req, c->sid, c->run_id, c->turn_id, c->id, c->name, JOB_TOOL, sandbox,
                              shared_keys.len ? buf_cstr(&shared_keys) : "",
                              xkeys.len ? buf_cstr(&xkeys) : "", now, err, sizeof err);
    }
    buf_free(&shared_keys);
    buf_free(&xkeys);
    cJSON_Delete(args);
    cJSON_Delete(pol);
    if (admitted) return;
    log_warn("无法执行 %s：%s", c->name, err);
    tool_done(k, c->sid, c->run_id, c->turn_id, c->id, false, err, now);
}


/* The node owns every managed process. A run owns only its foreground wait. */
static job_binding *binding_find(kernel_t *k,const char *id)
{
    if(!id)return NULL;
    for(job_binding *b=k->job_bindings;b;b=b->next)if(!strcmp(jobs_id(b->job),id))return b;
    return NULL;
}
static cJSON *job_public(job_binding *b)
{
    cJSON *state=jobs_snapshot(b->job);
    cJSON_DeleteItemFromObjectCaseSensitive(state,"request");
    cJSON_DeleteItemFromObjectCaseSensitive(state,"res_keys");
    cJSON_DeleteItemFromObjectCaseSensitive(state,"res_xkeys");
    /* Lease receipts contain immutable identity/resources, never command text. */
    return state;
}
static void managed_state(kernel_t *k,job_binding *b,const char *kind,const char *error)
{
    if(k->link.fd==NA_SOCK_NONE || !k->jobs_mode)return;
    cJSON *m=msg_new("job_state",NULL);
    cJSON_AddStringToObject(m,"node_epoch",k->node_epoch);
    cJSON_AddStringToObject(m,"job_id",jobs_id(b->job));
    cJSON_AddStringToObject(m,"kind",kind);
    if(error && *error)cJSON_AddStringToObject(m,"detail",error);
    cJSON_AddItemToObject(m,"job",job_public(b));
    if(jobs_finished(b->job))cJSON_AddNumberToObject(m,"event_seq",1);
    link_send(&k->link,m);
}
static void job_reply(kernel_t *k,const char *request,job_binding *b,bool ok,const char *detail,cJSON *data)
{
    cJSON *m=msg_new("job_state",NULL);
    cJSON_AddStringToObject(m,"node_epoch",k->node_epoch);
    cJSON_AddStringToObject(m,"request_id",request?request:"");
    cJSON_AddBoolToObject(m,"ok",ok);
    if(detail)cJSON_AddStringToObject(m,"detail",detail);
    if(b) {
        cJSON_AddStringToObject(m,"job_id",jobs_id(b->job));
        cJSON_AddItemToObject(m,"job",job_public(b));
    }
    if(data)cJSON_AddItemToObject(m,"job_data",data);
    link_send(&k->link,m);
}
static void job_control_reply(kernel_t *k,job_binding *b,bool ok,const char *detail)
{
    if(b->control_request)job_reply(k,b->control_request,b,ok,detail,NULL);
    free(b->control_request);b->control_request=NULL;
}
static void detach_send(kernel_t *k,job_binding *b,const char *phase,int64_t now)
{
    if(!k->detach_capable || !k->lease_synced || k->link.fd==NA_SOCK_NONE)return;
    char *type=xasprintf("lease_detach_%s",phase);
    cJSON *m=msg_new(type,NULL);free(type);
    cJSON_AddStringToObject(m,"node_epoch",k->node_epoch);
    cJSON_AddStringToObject(m,"lease_id",b->lease_id);
    cJSON_AddStringToObject(m,"request_id",jobs_id(b->job));
    link_send(&k->link,m);b->next_detach=now+2000;
}
static void managed_detach(kernel_t *k,job_binding *b,int64_t now)
{
    if(jobs_background(b->job)) {
        job_control_reply(k,b,true,"任务已经由后台 Job 持有");return;
    }
    if(b->wait_closed || jobs_finished(b->job) || b->process_drained) {
        b->detach_requested=false;
        job_control_reply(k,b,false,"任务已经结束");return;
    }
    if(!k->detach_capable || !k->jobs_mode || !b->lease_id) {
        job_control_reply(k,b,false,"执行节点没有协商后台移交能力");return;
    }
    b->detach_requested=true;
    if(b->detach_phase || !b->lease_registered)return;
    b->detach_phase=1;detach_send(k,b,"prepare",now);
}
static void managed_close_wait(kernel_t *k,job_binding *b,bool detached,int64_t now)
{
    if(b->wait_closed)return;
    b->wait_closed=true;
    cJSON *state=jobs_snapshot(b->job);
    const cJSON *result=cJSON_GetObjectItemCaseSensitive(state,"result");
    const char *status=sget(state,"status");
    bool ok=status && !strcmp(status,"succeeded");
    char *content=NULL;
    if(detached) {
        cJSON *receipt=cJSON_CreateObject();
        cJSON_AddStringToObject(receipt,"status","backgrounded");
        cJSON_AddStringToObject(receipt,"job_id",jobs_id(b->job));
        cJSON_AddStringToObject(receipt,"state",status?status:"unknown");
        cJSON_AddNullToObject(receipt,"exit_code");
        cJSON_AddStringToObject(receipt,"output_tail",sget(state,"output_tail")?sget(state,"output_tail"):"");
        content=cJSON_PrintUnformatted(receipt);cJSON_Delete(receipt);ok=true;
    } else {
        const char *text=sget(result,"content");
        if(!text)text=sget(state,"error");
        content=xstrdup(text?text:"执行没有完整终态");
    }
    cJSON *sandbox=cJSON_CreateObject();
    const cJSON *request=jobs_request(b->job);
    const char *requested=sget(request,"sandbox");
    cJSON_AddStringToObject(sandbox,"requested",requested?requested:"auto");
    cJSON_AddStringToObject(sandbox,"mode",effective_sandbox(k,requested));
    cJSON_AddStringToObject(sandbox,"effective",sget(state,"sandbox_effective")?sget(state,"sandbox_effective"):"unknown");
    if(b->kind==JOB_HOST) {
        cJSON *m=msg_new("run_result",NULL);
        cJSON_AddStringToObject(m,"id",b->id);cJSON_AddBoolToObject(m,"ok",ok);
        cJSON_AddStringToObject(m,"content",content);
        cJSON_AddStringToObject(m,"job_id",jobs_id(b->job));
        if(detached)cJSON_AddStringToObject(m,"status","backgrounded");
        cJSON_AddItemToObject(m,"sandbox",cJSON_Duplicate(sandbox,true));link_send(&k->link,m);
    } else if(detached) {
        na_event ev={.type=NA_EV_CALL_DETACHED,.sid=b->sid,.run_id=b->run_id,.turn_id=b->turn_id,
                     .id=b->id,.ok=true,.content=content};
        na_core_feed(k->core,now,&ev);
    } else tool_done_state(k,b->sid,b->run_id,b->turn_id,b->id,ok,content,sandbox,now);
    cJSON_Delete(sandbox);cJSON_Delete(state);free(content);
}
static void job_detach_result(kernel_t *k,const cJSON *m,int64_t now)
{
    if(!sget(m,"node_epoch") || strcmp(sget(m,"node_epoch"),k->node_epoch))return;
    job_binding *b=binding_find(k,sget(m,"request_id"));
    if(!b || !b->lease_id || !sget(m,"lease_id") || strcmp(b->lease_id,sget(m,"lease_id")))return;
    const char *phase=sget(m,"phase");
    bool ok=cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m,"ok"));
    const char *detail=sget(m,"detail");
    if(phase && !strcmp(phase,"prepare") && b->detach_phase==1) {
        if(!ok) {
            b->detach_phase=0;b->detach_requested=false;jobs_detach_denied(b->job);
            managed_state(k,b,"detach_denied",detail);job_control_reply(k,b,false,detail);return;
        }
        b->detach_phase=5; /* Wait for any preceding durable status write. */
    } else if(phase && !strcmp(phase,"commit") && b->detach_phase==3) {
        if(!ok) {
            /* Ownership is durable here: never roll back to an already-ended run. */
            managed_state(k,b,"detach_reconcile",detail);b->next_detach=now+5000;return;
        }
        b->detach_phase=0;managed_close_wait(k,b,true,now);
        managed_state(k,b,"detached",NULL);job_control_reply(k,b,true,"已转入后台，命令继续执行");
    } else if(phase && !strcmp(phase,"abort") && b->detach_phase==4 && ok) {
        b->detach_phase=0;b->detach_requested=false;
    }
}
static void sync_jobs(kernel_t *k)
{
    if(!k->jobs_mode)return;
    cJSON *m=msg_new("jobs_sync",NULL);
    cJSON_AddStringToObject(m,"node_epoch",k->node_epoch);
    cJSON *items=cJSON_AddArrayToObject(m,"jobs");
    for(job_binding *b=k->job_bindings;b;b=b->next)if(jobs_created(b->job))
        cJSON_AddItemToArray(items,job_public(b));
    link_send(&k->link,m);
}
static void binding_free(job_binding *b)
{
    if(b->process_drained && b->process)job_free(b->process);
    free(b->sid);free(b->run_id);free(b->id);free(b->lease_id);free(b->lease_spec);
    free(b->control_request);free(b->read_request);free(b);
}
static void pump_jobs(kernel_t *k,int64_t now)
{
    if(!k->jobs)return;
    job_event event;
    while(jobs_poll(k->jobs,now,&event)) {
        job_binding *b=binding_find(k,event.id);
        if(!b){jobs_event_free(&event);continue;}
        if(!strcmp(event.kind,"removed")) {
            b->retention_pending=false;
            if(!event.ok)b->next_prune=now+5000;
            if(event.ok) {
                job_binding **pp=&k->job_bindings;
                while(*pp && *pp!=b)pp=&(*pp)->next;
                if(*pp)*pp=b->next;
                managed_job *owned=b->job;binding_free(b);jobs_forget(owned);
            } else managed_state(k,b,"storage_error",event.error);
            jobs_event_free(&event);continue;
        }
        if(!strcmp(event.kind,"created")) {
            if(event.ok) {
                managed_state(k,b,"queued",NULL);
                const cJSON *args=cJSON_GetObjectItemCaseSensitive(jobs_request(b->job),"args");
                if(sget(args,"execution") && !strcmp(sget(args,"execution"),"background"))managed_detach(k,b,now);
            } else {
                jobs_fail(b->job,"failed",event.error);
                managed_close_wait(k,b,false,now);
                managed_state(k,b,"storage_error",event.error);
            }
        } else if(!strcmp(event.kind,"start") && !event.ok) {
            jobs_fail(b->job,"failed",event.error);
        } else if(!strcmp(event.kind,"started")) {
            b->started_at=now;lease_send(k,"lease_started",b->lease_id);
            cJSON *state=jobs_snapshot(b->job);
            const cJSON *timeout=cJSON_GetObjectItemCaseSensitive(state,"timeout_seconds");
            int64_t duration=cJSON_IsNumber(timeout)?(int64_t)timeout->valueint*1000:1800000;
            if(!b->wait_closed && b->kind==JOB_TOOL) {
                na_event ev={.type=NA_EV_CALL_STARTED,.sid=b->sid,.run_id=b->run_id,
                    .turn_id=b->turn_id,.id=b->id,.started_timeout_ms=duration>0?duration:-1};
                na_core_feed(k->core,now,&ev);
            } else if(!b->wait_closed) {
                cJSON *out=msg_new("run_started",NULL);cJSON_AddStringToObject(out,"id",b->id);
                cJSON_AddStringToObject(out,"job_id",jobs_id(b->job));
                cJSON_AddNumberToObject(out,"timeout_ms",(double)duration);link_send(&k->link,out);
            }
            cJSON_Delete(state);
        } else if(!strcmp(event.kind,"detach_ready")) {
            managed_detach(k,b,now);
        } else if(!strcmp(event.kind,"detached")) {
            if(event.ok) {
                if(b->process)b->process->detached=true;
                b->detach_phase=3;detach_send(k,b,"commit",now);
            } else {
                b->detach_phase=4;b->detach_requested=false;detach_send(k,b,"abort",now);
                job_control_reply(k,b,false,event.error);
                managed_state(k,b,"storage_error",event.error);
            }
        } else if(!strcmp(event.kind,"terminal") && event.ok) {
            managed_state(k,b,"terminal",NULL);b->next_event=now+5000;
        } else if(!strcmp(event.kind,"output")) {
            if(b->read_request && b->read_id==event.request_id) {
                job_reply(k,b->read_request,b,event.ok,event.error,cJSON_Duplicate(event.data,true));
                free(b->read_request);b->read_request=NULL;
            }
        } else if(!event.ok)managed_state(k,b,"storage_error",event.error);
        else if(!strcmp(event.kind,"running") || !strcmp(event.kind,"cancelling"))managed_state(k,b,event.kind,NULL);
        else if(!strcmp(event.kind,"ack"))managed_state(k,b,"ack",NULL);
        jobs_event_free(&event);
    }
    for(job_binding *b=k->job_bindings;b;b=b->next) {
        if(b->detach_requested && !b->detach_phase && !jobs_background(b->job))managed_detach(k,b,now);
        if(b->detach_phase==5) {
            char error[512]="";
            if(jobs_detach(b->job,++k->job_request_seq,error,sizeof error))b->detach_phase=2;
            else if(jobs_finished(b->job) || b->process_drained) {
                b->detach_phase=4;detach_send(k,b,"abort",now);
                job_control_reply(k,b,false,"任务已经结束");
            }
        }
        if(b->detach_phase && now>=b->next_detach) {
            if(b->detach_phase==1)detach_send(k,b,"prepare",now);
            if(b->detach_phase==3)detach_send(k,b,"commit",now);
            if(b->detach_phase==4)detach_send(k,b,"abort",now);
        }
        if(jobs_finished(b->job) && !b->terminal_handled && !b->recovered) {
            /* Durable detach wins even if exit follows before the commit reply. */
            if(!jobs_background(b->job))managed_close_wait(k,b,false,now);
            if(b->detach_phase==3 || b->detach_phase==2 || b->detach_phase==5)continue;
            cJSON *state=jobs_snapshot(b->job);
            const char *status=sget(state,"status");
            if(status && !strcmp(status,"unknown")) {
                if(!b->unknown_reported)lease_send(k,"lease_cancel",b->lease_id);
                b->unknown_reported=true;cJSON_Delete(state);continue;
            }
            cJSON_Delete(state);
            lease_send(k,"lease_release",b->lease_id);b->terminal_handled=true;
            if(b->process) {
                if(b->process->tag==1 && k->nshells>0)k->nshells--;
                job_free(b->process);b->process=NULL;
            }
        }
        if(jobs_finished(b->job) && now>=b->next_event) {
            cJSON *state=jobs_snapshot(b->job);
            const char *notification=sget(state,"notification_state");
            if(notification && !strcmp(notification,"pending"))managed_state(k,b,"terminal",NULL);
            cJSON_Delete(state);b->next_event=now+5000;
        }
        if(!b->long_notice && !b->wait_closed && b->started_at && now-b->started_at>=10000) {
            managed_state(k,b,"long_running","命令仍在执行，可以 /bg 转后台、/job 查看输出或停止");
            b->long_notice=true;
        }
    }
    /* At 240 records, reclaim only acknowledged definitive old terminals
     * until 32 slots are free. Pending outbox and unknown ownership never expire. */
    if(jobs_count(k->jobs)>=240)k->jobs_pruning=true;
    if(jobs_count(k->jobs)<=224)k->jobs_pruning=false;
    if(k->jobs_pruning) {
        size_t pending=0;
        for(job_binding *b=k->job_bindings;b;b=b->next)if(b->retention_pending)pending++;
        for(unsigned submitted=0;submitted<8 && jobs_count(k->jobs)>224+pending;submitted++) {
            job_binding *oldest=NULL;double oldest_at=0;
            for(job_binding *b=k->job_bindings;b;b=b->next) {
                if(!b->terminal_handled || b->process || b->read_request || b->control_request || b->retention_pending || now<b->next_prune)continue;
                double at=0;
                if(jobs_removable(b->job,&at) && (!oldest || at<oldest_at ||
                   (at==oldest_at && strcmp(jobs_id(b->job),jobs_id(oldest->job))<0))) {
                    oldest=b;oldest_at=at;
                }
            }
            if(!oldest || !jobs_remove(oldest->job,++k->job_request_seq))break;
            oldest->retention_pending=true;pending++;
        }
    }
    /* Closed queue entries cannot be allowed to launch after a cancellation fence. */
    execq_t **pp=&k->execq;
    while(*pp) {
        execq_t *q=*pp;
        if(q->managed && (jobs_finished(q->managed->job) || (q->managed->wait_closed && !jobs_background(q->managed->job)))) {
            *pp=q->next;execq_free(q);
        } else pp=&q->next;
    }
}
static void job_control(kernel_t *k,const cJSON *m,int64_t now)
{
    const char *request=sget(m,"request_id"),*action=sget(m,"action");
    if(!request || !*request || strlen(request)>128 || !action)return;
    if(!k->jobs || !k->jobs_mode) {job_reply(k,request,NULL,false,"节点后台 Job 能力不可用",NULL);return;}
    if(!strcmp(action,"list")) {
        cJSON *items=cJSON_CreateArray();
        for(job_binding *b=k->job_bindings;b;b=b->next)if(jobs_created(b->job))cJSON_AddItemToArray(items,job_public(b));
        job_reply(k,request,NULL,true,NULL,items);return;
    }
    job_binding *b=binding_find(k,sget(m,"job_id"));
    if(!b || !jobs_created(b->job)){job_reply(k,request,NULL,false,"任务不存在或尚未可靠保存",NULL);return;}
    if(!strcmp(action,"status"))job_reply(k,request,b,true,NULL,NULL);
    else if(!strcmp(action,"output")) {
        const cJSON *cursor=cJSON_GetObjectItemCaseSensitive(m,"cursor");
        const cJSON *maximum=cJSON_GetObjectItemCaseSensitive(m,"max_bytes");
        if((cursor && (!cJSON_IsNumber(cursor) || cursor->valuedouble<0 || cursor->valuedouble>9007199254740991.0 ||
                       cursor->valuedouble!=(double)(uint64_t)cursor->valuedouble)) ||
           (maximum && (!cJSON_IsNumber(maximum) || maximum->valuedouble<1 || maximum->valuedouble>65536))) {
            job_reply(k,request,b,false,"日志游标或字节上限无效",NULL);return;
        }
        if(b->read_request) {job_reply(k,request,b,false,"本任务已有日志读取正在进行",NULL);return;}
        uint64_t sequence=++k->job_request_seq;
        if(!jobs_read(b->job,cursor?(uint64_t)cursor->valuedouble:0,maximum?(size_t)maximum->valueint:16384,sequence)) {
            job_reply(k,request,b,false,"日志读取队列繁忙",NULL);return;
        }
        b->read_id=sequence;b->read_request=xstrdup(request);
    } else if(!strcmp(action,"detach")) {
        if(b->control_request && strcmp(b->control_request,request)) {job_reply(k,request,b,false,"移交正在进行",NULL);return;}
        if(!b->control_request)b->control_request=xstrdup(request);
        managed_detach(k,b,now);
    } else if(!strcmp(action,"cancel")) {
        if(b->recovered && !b->terminal_handled) {job_reply(k,request,b,false,"节点重启后状态未知，不能确认旧进程已停止",NULL);return;}
        if(jobs_finished(b->job)) {job_reply(k,request,b,true,"任务已结束",NULL);return;}
        jobs_cancelling(b->job);
        if(b->process) {
            runner_stop(b->process);
            if(!b->detach_phase)lease_send(k,"lease_cancel",b->lease_id);
        }
        else {
            jobs_fail(b->job,"cancelled","用户停止，未启动命令");
            execq_t **pp=&k->execq;
            while(*pp && (*pp)->managed!=b)pp=&(*pp)->next;
            if(*pp){execq_t *q=*pp;*pp=q->next;execq_free(q);}
        }
        job_reply(k,request,b,true,"已请求停止，等待进程回收与终态保存",NULL);
    } else job_reply(k,request,b,false,"未知 Job 操作",NULL);
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

static void handle_run(kernel_t *k, const cJSON *m, int64_t now)
{
    const char *id = sget(m, "id");
    for(job_binding *b=k->job_bindings;b;b=b->next)
        if(id && b->kind==JOB_HOST && !strcmp(id,b->id))return;
    const cJSON *req = cJSON_GetObjectItemCaseSensitive(m, "req");
    const char *tool = sget(req, "tool");
    char err[1024] = "";
    if (!id || !cJSON_IsObject(req) || !tool) {
        send_run_result(k, id, false, "执行请求不完整");
        return;
    }
    /* 代执行也走同一个入口：同样的执行槽、同样的资源锁、同样的排队，不绕过机器限额 */
    const cJSON *args = cJSON_GetObjectItemCaseSensitive(req, "args");
    cJSON *ws = cJSON_CreateObject();
    const cJSON *dirs = cJSON_GetObjectItemCaseSensitive(req, "dirs");
    cJSON_AddItemToObject(ws, "dirs", cJSON_IsArray(dirs) ? cJSON_Duplicate(dirs, 1) : cJSON_CreateArray());
    buf_t shared = {0}, xkeys = {0};
    const char *sandbox = sget(req, "sandbox");
    if (cJSON_IsObject(args)) call_resource(tool, args, ws, !strcmp(effective_sandbox(k, sandbox), "off"), &shared, &xkeys);
    bool ok = exec_admit(k, cJSON_Duplicate(req, 1), "", "", 0, id, tool, JOB_HOST, sandbox,
                         shared.len ? buf_cstr(&shared) : "", xkeys.len ? buf_cstr(&xkeys) : "",
                         now, err, sizeof err);
    buf_free(&shared);
    buf_free(&xkeys);
    cJSON_Delete(ws);
    if (!ok) {
        send_run_result(k, id, false, err);
        return;
    }
    log_info("代网关执行 %s（%s）", tool, id);
}

/* 技能文件放在 NANO_HOME/skill-cache/ 下，不能覆盖 skills/ 中的源文件。 */
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
    char *nh = plat_getenv_utf8("NANO_HOME");
    if (!nh && errno != ENOENT) { snprintf(err, errlen, "读取 NANO_HOME 的 Unicode 值失败"); free(home); return false; }
    char *base = nh && plat_is_abs(nh) ? xstrdup(nh) : plat_join(home, ".nano-agent");
    free(nh);
    char *root = plat_join(base, "skill-cache");
    char *dir = plat_join(root, name);
    char *tmpname = xasprintf(".%s.tmp", name);
    char *tmp = plat_join(root, tmpname);
    free(tmpname);
    free(base);

    bool ok = plat_makedirs(root) == 0;
    if (ok) plat_remove_tree(tmp);
    ok = ok && plat_mkdir(tmp) == 0;
    if (!ok) {
        char *detail = plat_failure_detail("kernel.skill_cache.prepare", tmp);
        snprintf(err, errlen, "建不了目录：%s", detail);
        free(detail);
    }
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

static void handle_msg(kernel_t *k, const cJSON *m, int64_t now);

static int pendin_len(const kernel_t *k)
{
    int n = 0;
    for (const pendin_t *p = k->pendin; p; p = p->next) n++;
    return n;
}

static void pendin_push(kernel_t *k, const cJSON *m)
{
    pendin_t *p = xcalloc(1, sizeof *p);
    p->m = cJSON_Duplicate(m, 1);
    pendin_t **pp = &k->pendin;
    while (*pp) pp = &(*pp)->next;
    *pp = p;
}

/* 排队的 input 太多：丢掉最早的那个（有界，不能让无限输入把内存吃光） */
static void pendin_drop_oldest(kernel_t *k)
{
    pendin_t *p = k->pendin;
    if (!p) return;
    k->pendin = p->next;
    log_warn("排队的 input 到上限，丢掉最早的一个（sid=%s）", cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(p->m, "sid")));
    cJSON_Delete(p->m);
    free(p);
}

static void pendin_clear(kernel_t *k)
{
    while (k->pendin) {
        pendin_t *p = k->pendin;
        k->pendin = p->next;
        cJSON_Delete(p->m);
        free(p);
    }
}

/* 有 run 结束了就放一个排队的 input 进来 */
static void feed_pending_input(kernel_t *k, int64_t now)
{
    while (k->pendin && na_core_active_runs(k->core) < (size_t)na_core_max_runs(k->core)) {
        pendin_t *p = k->pendin;
        k->pendin = p->next;
        log_info("放行一个排队的 input");
        handle_msg(k, p->m, now);
        cJSON_Delete(p->m);
        free(p);
    }
}

static void pump(kernel_t *k, int64_t now)
{
    na_cmd *c;
    while ((c = na_core_next(k->core))) {
        cJSON *m = NULL;
        switch (c->type) {
        case NA_CMD_MODEL:
            m = msg_new("model", c->sid);
            if (c->run_id && *c->run_id) cJSON_AddStringToObject(m, "run_id", c->run_id);
            cJSON_AddNumberToObject(m, "turn_id", c->turn_id);
            break;
        case NA_CMD_RESULT:
            m = msg_new("result", c->sid);
            cJSON_AddStringToObject(m, "id", c->id);
            if (c->run_id && *c->run_id) cJSON_AddStringToObject(m, "run_id", c->run_id);
            if (c->turn_id) cJSON_AddNumberToObject(m, "turn_id", c->turn_id);
            cJSON_AddBoolToObject(m, "ok", c->ok);
            if (c->execution) {
                cJSON *state = cJSON_Parse(c->execution);
                if (state) cJSON_AddItemToObject(m, "sandbox", state);
            }
            cJSON_AddStringToObject(m, "content", c->text ? c->text : "");
            if (!c->ok) log_info("调用 %s 没有成功：%.*s", c->id, (int)utf8_cut(c->text ? c->text : "", c->text ? strlen(c->text) : 0, 200), c->text ? c->text : "");
            break;
        case NA_CMD_QUEUED:
            m = msg_new("tool_queued", c->sid);
            cJSON_AddStringToObject(m, "id", c->id);
            cJSON_AddStringToObject(m, "name", c->name ? c->name : "");
            if (c->run_id && *c->run_id) cJSON_AddStringToObject(m, "run_id", c->run_id);
            cJSON_AddNumberToObject(m, "turn_id", c->turn_id);
            cJSON_AddStringToObject(m, "text", c->text ? c->text : "");
            break;
        case NA_CMD_APPROVE_REQ: {
            m = msg_new("approve_req", c->sid);
            cJSON_AddStringToObject(m, "id", c->id);
            if (c->run_id && *c->run_id) cJSON_AddStringToObject(m, "run_id", c->run_id);
            cJSON_AddNumberToObject(m, "turn_id", c->turn_id);
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
            if (c->run_id && *c->run_id) cJSON_AddStringToObject(m, "run_id", c->run_id);
            if (c->text) cJSON_AddStringToObject(m, "detail", c->text);
            log_info("run 结束 sid=%s status=%s", c->sid, c->status);
            break;
        case NA_CMD_EXEC:
            exec_tool(k, c, now);   /* 失败时会再喂一个事件，产生的命令由本循环继续处理 */
            break;
        case NA_CMD_KILL: {
            gwjob_t *g = gw_take(k, c->sid, c->run_id, c->turn_id, c->id);
            if (g) {
                /* 交给网关执行的：请网关停下 */
                m = msg_new("exec_cancel", c->sid);
                cJSON_AddStringToObject(m, "id", c->id);
                cJSON_AddStringToObject(m, "run_id", c->run_id ? c->run_id : "");
                cJSON_AddNumberToObject(m, "turn_id", c->turn_id);
                gw_free(g);
            } else if (!execq_drop(k, c->sid, c->id)) {
                for(job_binding *b=k->job_bindings;b;b=b->next)
                    if(!strcmp(b->sid,c->sid) && !strcmp(b->id,c->id) &&
                       !strcmp(b->run_id,c->run_id?c->run_id:"") && b->turn_id==c->turn_id &&
                       !jobs_background(b->job))jobs_cancelling(b->job);
                runner_kill(&k->runner, c->sid, c->id);
            }
            log_info("终止 %s", c->id);
            break;
        }
        }
        if (m) link_send(&k->link, m);
        na_cmd_free(c);
    }
    feed_pending_input(k, now);
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
static char *build_policy(const cJSON *m, na_mode mode, const char *sandbox)
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
    /* 这一轮最终的沙箱模式：执行时按它交给执行器，也决定“没有隔离时要不要审批” */
    cJSON_AddStringToObject(p, "sandbox", sandbox && *sandbox ? sandbox : "auto");
    if (sget(m, "owner_agent")) cJSON_AddStringToObject(p, "owner_agent", sget(m, "owner_agent"));
    const cJSON *priority = cJSON_GetObjectItemCaseSensitive(m, "priority");
    if (cJSON_IsNumber(priority)) cJSON_AddNumberToObject(p, "priority", priority->valueint);
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
    for(job_binding *b=k->job_bindings;b;b=b->next)if(!jobs_finished(b->job)) {
        cJSON *reply=msg_new("update_result",NULL);cJSON_AddBoolToObject(reply,"ok",false);
        cJSON_AddStringToObject(reply,"detail","节点仍有 Job，延后自动更新；主动重启会中断任务");
        if(ver)cJSON_AddStringToObject(reply,"version",ver);
        link_send(&k->link,reply);return;
    }
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
        const cJSON *gv = cJSON_GetObjectItemCaseSensitive(m, "v");
        k->gw_ver = cJSON_IsNumber(gv) ? gv->valueint : 10;
        log_info("网关已接受连接（协议 v%d）", k->gw_ver);
        const cJSON *features = cJSON_GetObjectItemCaseSensitive(m, "features");
        const cJSON *leases = cJSON_GetObjectItemCaseSensitive(features, "machine_leases");
        k->leasing = cJSON_IsNumber(leases) && leases->valueint >= 1;
        k->lease_synced = !k->leasing;
        const cJSON *detach=cJSON_GetObjectItemCaseSensitive(features,"lease_detach");
        const cJSON *manager=cJSON_GetObjectItemCaseSensitive(features,"job_manager");
        k->detach_capable=k->leasing && cJSON_IsNumber(detach) && detach->valueint>=1;
        k->jobs_mode=k->jobs && k->exec_stream_version>=1 && k->detach_capable &&
                     cJSON_IsNumber(manager) && manager->valueint>=1;
        if (k->leasing) lease_sync(k, now);
        if(k->jobs_mode)sync_jobs(k);
        set_state(k, "connected", NULL, NULL);
        if (k->update_deadline) {
            update_confirm();
            k->update_deadline = 0;
            log_info("更新到 %s 成功", NA_VERSION);
        }
        return;
    }
    if(!strcmp(t,"lease_detach_result")) {job_detach_result(k,m,now);return;}
    if(!strcmp(t,"job_control")) {job_control(k,m,now);return;}
    if(!strcmp(t,"events_ack")) {
        const cJSON *seq=cJSON_GetObjectItemCaseSensitive(m,"event_seq");
        managed_job *owned=k->jobs && sget(m,"job_id")?jobs_find(k->jobs,sget(m,"job_id")):NULL;
        if(owned && cJSON_IsNumber(seq) && seq->valuedouble==1)jobs_acknowledge(owned,1);
        return;
    }
    if (!strcmp(t, "lease_synced")) {
        if (!sget(m, "node_epoch") || strcmp(sget(m, "node_epoch"), k->node_epoch)) return;
        k->lease_synced = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "ok"));
        if (k->lease_synced) {
            set_state(k, "connected", NULL, NULL);
            for (execq_t *q = k->execq; q; q = q->next) { q->lease_asked = false; q->lease_granted = false; lease_ask(k, q); }
            pump_execq(k, now);
        } else set_state(k, "blocked", sget(m, "detail"), "lease_sync");
        return;
    }
    if (!strcmp(t, "lease_grant") || !strcmp(t, "lease_deny") || !strcmp(t, "lease_queued")) {
        if (!sget(m, "node_epoch") || strcmp(sget(m, "node_epoch"), k->node_epoch)) return;
        const char *id = sget(m, "lease_id");
        if (!id) return;
        execq_t **pp = &k->execq;
        while (*pp && (!(*pp)->lease_id || strcmp((*pp)->lease_id, id))) pp = &(*pp)->next;
        if (!*pp) {
            for (job_t *j = k->runner.jobs; j; j = j->next) if (j->lease_id && !strcmp(j->lease_id, id)) return;
            if (!strcmp(t, "lease_grant")) lease_send(k, "lease_release", id);
            return;
        }
        execq_t *q = *pp;
        if(q->managed && strcmp(t,"lease_deny")) {
            q->managed->lease_registered=true;
            if(q->managed->detach_requested)managed_detach(k,q->managed,now);
        }
        if (!strcmp(t, "lease_grant")) { q->lease_granted = true; pump_execq(k, now); }
        else if (!strcmp(t, "lease_deny")) {
            *pp = q->next;
            if(q->managed)jobs_fail(q->managed->job,"failed",sget(m,"detail"));
            else if (q->kind == JOB_TOOL) tool_done(k, q->sid, q->run_id, q->turn_id, q->id, false, sget(m, "detail"), now);
            else send_run_result(k, q->id, false, sget(m, "detail"));
            execq_free(q);
        } else if (q->kind == JOB_TOOL) {
            cJSON *out = msg_new("tool_queued", q->sid);
            cJSON_AddStringToObject(out, "run_id", q->run_id);cJSON_AddNumberToObject(out, "turn_id", q->turn_id);
            cJSON_AddStringToObject(out, "id", q->id);cJSON_AddStringToObject(out, "name", q->name);
            cJSON_AddStringToObject(out, "text", sget(m, "detail") ? sget(m, "detail") : "等执行槽或资源");
            link_send(&k->link, out);
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
    if (!strcmp(t,"exec_queued")) {
        const char *sid=sget(m,"sid"), *id=sget(m,"id"), *run_id=sget(m,"run_id");
        const cJSON *v=cJSON_GetObjectItemCaseSensitive(m,"turn_id");
        int turn_id=cJSON_IsNumber(v)?v->valueint:0;
        gwjob_t *g=gw_find(k,sid,run_id,turn_id,id);
        if (g) call_queued(k,g->sid,g->run_id,g->turn_id,g->id,"",now);
        return;
    }
    if (!strcmp(t, "exec_started")) {
        /* 网关那边真的开始执行了：派发等待结束，执行时限从现在算。
         * 条目留在表里，终态结果还要用它配对。 */
        const cJSON *gid = cJSON_GetObjectItemCaseSensitive(m, "turn_id");
        gwjob_t *g = gw_find(k, sget(m, "sid"), sget(m, "run_id"), cJSON_IsNumber(gid) ? gid->valueint : 0,
                             sget(m, "id"));
        if (!g || g->started) return;
        g->started = true;
        call_started(k, g->sid, g->run_id, g->turn_id, g->id, now);
        return;
    }
    if (!strcmp(t, "exec_result")) {
        const cJSON *gid = cJSON_GetObjectItemCaseSensitive(m, "turn_id");
        gwjob_t *g = gw_take(k, sget(m, "sid"), sget(m, "run_id"), cJSON_IsNumber(gid) ? gid->valueint : 0,
                             sget(m, "id"));
        if (!g) return;   /* 已经超时、被停止，或者身份对不上（上一轮的迟到结果） */
        bool ok = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "ok"));
        log_info("网关执行 %s %s", g->id, ok ? "完成" : "失败");
        tool_done_state(k, g->sid, g->run_id, g->turn_id, g->id, ok, sget(m, "content") ? sget(m, "content") : "", cJSON_GetObjectItemCaseSensitive(m, "sandbox"), now);
        gw_free(g);
        return;
    }
    if (!strcmp(t, "run")) {
        handle_run(k, m, now);
        return;
    }
    if (!strcmp(t, "run_cancel")) {
        /* 还没启动的先从队列里拿掉：光杀进程没用，排队的请求会在槽位空出来之后自己跑起来 */
        const char *cid = sget(m, "id");
        if (!execq_drop(k, "", cid)) {
            for(job_binding *b=k->job_bindings;b;b=b->next)
                if(cid && b->kind==JOB_HOST && !strcmp(b->id,cid) && !jobs_background(b->job))jobs_cancelling(b->job);
            runner_kill(&k->runner, "", cid);
        }
        log_info("取消代执行 %s", cid ? cid : "?");
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
    char **reskeys = NULL;
    char *policy = NULL;
    cJSON *run_pol = NULL;
    size_t ncalls = 0;

    if (!strcmp(t, "input")) {
        /* 活跃 run 到了上限：先排队（有界），等这个内核上的 run 结束再喂进去。
         * 网关那边只是暂时收不到 MODEL，不会被无限分配拖垮。 */
        if (na_core_active_runs(k->core) >= (size_t)na_core_max_runs(k->core)) {
            if (pendin_len(k) >= PENDIN_MAX) pendin_drop_oldest(k);
            pendin_push(k, m);
            log_info("活跃 run 已满（%d 个），input 先排队", na_core_max_runs(k->core));
            return;
        }
        ev.type = NA_EV_INPUT;
        ev.run_id = sget(m, "run_id");
        ev.from = sget(m, "from");
        ev.mode = parse_mode(sget(m, "mode"));
        /* 网关的设置：大于 0 是上限，小于 0 不限，没有（旧网关）用 kernel.conf */
        const cJSON *ms = cJSON_GetObjectItemCaseSensitive(m, "max_steps");
        const cJSON *rt = cJSON_GetObjectItemCaseSensitive(m, "run_timeout_ms");
        const cJSON *at = cJSON_GetObjectItemCaseSensitive(m, "approval_timeout_ms");
        const cJSON *mp = cJSON_GetObjectItemCaseSensitive(m, "max_parallel_tools");
        ev.max_steps = cJSON_IsNumber(ms) ? ms->valueint : 0;
        ev.run_timeout_ms = cJSON_IsNumber(rt) ? rt->valueint : 0;
        ev.approval_timeout_ms = cJSON_IsNumber(at) ? at->valueint : 0;
        ev.max_parallel_tools = cJSON_IsNumber(mp) ? mp->valueint : 0;
        ev.elevate = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "elevate"));
        /* 这一轮最终的沙箱模式：节点自己的 on/off 优先，其余听网关的请求 */
        const char *sb = effective_sandbox(k, sget(m, "sandbox_mode"));
        ev.sandbox = !strcmp(sb, "off") ? NA_SB_OFF : k->landlock > 0 ? NA_SB_YES : NA_SB_NO;
        policy = build_policy(m, ev.mode, sget(m, "sandbox_mode"));
        ev.policy = policy;
        const cJSON *ws = cJSON_GetObjectItemCaseSensitive(m, "workspaces");
        log_info("run 开始 sid=%s from=%s mode=%s 工作目录 %d 个", ev.sid ? ev.sid : "-", ev.from ? ev.from : "-",
                 sget(m, "mode") ? sget(m, "mode") : "none", cJSON_IsArray(ws) ? cJSON_GetArraySize(ws) : 0);
    } else if (!strcmp(t, "turn")) {
        ev.type = NA_EV_TURN;
        ev.run_id = sget(m, "run_id");
        const cJSON *tid = cJSON_GetObjectItemCaseSensitive(m, "turn_id");
        ev.turn_id = cJSON_IsNumber(tid) ? tid->valueint : 0;
        ev.reason = parse_reason(sget(m, "reason"));
        ev.detail = sget(m, "detail");
        const cJSON *arr = cJSON_GetObjectItemCaseSensitive(m, "calls");
        ncalls = cJSON_IsArray(arr) ? (size_t)cJSON_GetArraySize(arr) : 0;
        if (ncalls > MAX_CALLS_WIRE) {
            ev.reason = NA_TURN_ERROR;
            ev.detail = "工具提案超过协议边界，整批没有接受或执行";
            ncalls = 0;
        }
        if (ncalls) {
            calls = xcalloc(ncalls, sizeof *calls);
            owned = xcalloc(ncalls * 2, sizeof *owned);   /* 参数文本、预检的错误 */
            reskeys = xcalloc(ncalls * 2, sizeof *reskeys);   /* 共享键、独占键 */
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
            /* MCP 工具：默认每个服务串行（服务自己能证明独立时由网关放宽） */
            if (calls[i].name && mcp_tool(calls[i].name)) {
                const char *sep = strstr(calls[i].name, "__");
                buf_t mk = {0};
                buf_puts(&mk, "mcp:");
                buf_append(&mk, calls[i].name, (size_t)(sep - calls[i].name));
                reskeys[ncalls + i] = buf_cstr(&mk);
                calls[i].res_xkeys = reskeys[ncalls + i];   /* 每个服务默认串行 */
            }
            /* 先找好工作目录：写错了直接告诉模型；有没有沙箱按那台机器算 */
            if (calls[i].args_ok && calls[i].name && !gateway_tool(calls[i].name) && !mcp_tool(calls[i].name) && run_pol) {
                char err[1024];
                const cJSON *ws = pick_workspace(run_pol, args, err, sizeof err);
                if (!ws) {
                    owned[ncalls + i] = xstrdup(err);
                    calls[i].error = owned[ncalls + i];
                } else {
                    bool local = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(ws, "local"));
                    const cJSON *ll = cJSON_GetObjectItemCaseSensitive(ws, "landlock");
                    int landlock = local ? k->landlock : cJSON_IsNumber(ll) ? ll->valueint : 0;
                    /* 明确关掉隔离的（这一轮 off，或者那台机器上的内核也是 off）不再退回审批；
                     * 其余按那台机器有没有沙箱算 */
                    const char *want = sget(ws, "sandbox_mode");
                    if (!want) want = sget(run_pol, "sandbox");
                    const char *required = local ? k->conf.sandbox_mode : sget(ws, "sandbox_required");
                    const char *effective = required && (!strcmp(required, "on") || !strcmp(required, "off")) ? required : want;
                    int mode = effective && !strcmp(effective, "off") ? SB_OFF : effective && !strcmp(effective, "on") ? SB_ON : SB_AUTO;
                    if ((want && !strcmp(want, "on") && required && !strcmp(required, "off")) || (mode == SB_ON && landlock <= 0)) {
                        owned[ncalls + i] = xstrdup("目标机器不能满足 sandbox=on（不支持完整隔离或节点部署要求为 off），没有执行");
                        calls[i].error = owned[ncalls + i];
                    }
                    calls[i].sandbox = mode == SB_OFF ? NA_SB_OFF : landlock > 0 ? NA_SB_YES : NA_SB_NO;
                    /* 并发分类：按真工具、真参数算，交给状态机排队 */
                    buf_t shared = {0}, xkeys = {0};
                    call_resource(calls[i].name, args, ws, mode == SB_OFF, &shared, &xkeys);
                    if (shared.len) {
                        reskeys[i] = buf_cstr(&shared);
                        calls[i].res_keys = reskeys[i];
                    } else {
                        buf_free(&shared);
                    }
                    if (xkeys.len) {
                        reskeys[ncalls + i] = buf_cstr(&xkeys);
                        calls[i].res_xkeys = reskeys[ncalls + i];
                    } else {
                        buf_free(&xkeys);
                    }
                }
            }
            log_info("模型请求 %s（%s）", calls[i].name ? calls[i].name : "?", calls[i].id ? calls[i].id : "?");
        }
        ev.calls = calls;
        ev.ncalls = ncalls;
    } else if (!strcmp(t, "approve")) {
        ev.type = NA_EV_APPROVE;
        ev.run_id = sget(m, "run_id");
        const cJSON *tid = cJSON_GetObjectItemCaseSensitive(m, "turn_id");
        ev.turn_id = cJSON_IsNumber(tid) ? tid->valueint : 0;
        ev.id = sget(m, "id");
        ev.ok = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(m, "ok"));
        ev.from = sget(m, "from");
    } else if (!strcmp(t, "cancel")) {
        ev.type = NA_EV_CANCEL;
    } else if (!strcmp(t, "model_yield")) {
        /* 网关：这次生成完整结束，但要先插入用户补充、重新请求模型。轮次预算还是内核管。 */
        ev.type = NA_EV_MODEL_YIELD;
    } else {
        log_debug("忽略未知消息：%s", t);
        return;
    }

    if (na_core_feed(k->core, now, &ev) < 0)
        log_warn("忽略与当前状态不符的消息：%s sid=%s", t, ev.sid ? ev.sid : "-");

    for (size_t i = 0; i < ncalls; i++) {
        cJSON_free(owned[i]);
        free(owned[ncalls + i]);
        free(reskeys ? reskeys[i] : NULL);
        free(reskeys ? reskeys[ncalls + i] : NULL);
    }
    free(owned);
    free(reskeys);
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
    k->lease_synced = false;
    k->gw_ver = 0;
    /* 网关那边已经不在了：把它让起的 MCP 进程都收掉，别留在机器上。
       用 reset 不用 shutdown：这一轮主循环后面的 pipes_poll 还拿着这些条目的引用 */
    pipes_reset(&k->pipes);
    na_event ev = { .type = NA_EV_RESET };
    na_core_feed(k->core, now, &ev);
    for(job_binding *b=k->job_bindings;b;b=b->next) {
        if(!jobs_background(b->job))b->wait_closed=true;
        free(b->control_request);b->control_request=NULL;
        free(b->read_request);b->read_request=NULL;
        if(b->detach_phase && jobs_background(b->job))b->detach_phase=3;
        else if(b->detach_phase!=2)b->detach_phase=0;
    }
    execq_clear(k);
    runner_kill_foreground(&k->runner);
    pendin_clear(k);
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
        if(j->managed) {
            job_binding *b=j->managed;
            cJSON *receipt=runner_result_object(j);
            if(j->killed && !receipt)jobs_fail(b->job,"cancelled","停止后进程已回收，执行器未保留完整终态");
            else if(!receipt)jobs_fail(b->job,"unknown","执行器流式协议没有完整且有效终态，不能确认命令结果");
            else jobs_process_finished(b->job);
            cJSON_Delete(receipt);b->process_drained=true;
            /* Keep the real lease and binding until terminal metadata is durable. */
            continue;
        }
        if (!j->killed) {
            bool ok;
            char *res = job_result(j, &ok);
            log_info("%s（%s）%s", j->name, j->id, ok ? "完成" : "失败");
            cJSON *raw = cJSON_Parse(buf_cstr(&j->out));
            cJSON *state = cJSON_CreateObject();
            cJSON_AddStringToObject(state, "mode", j->sandbox ? j->sandbox : "auto");
            cJSON_AddStringToObject(state, "requested", j->sandbox_requested ? j->sandbox_requested : "auto");
            const char *effective = sget(raw, "sandbox_effective");
            cJSON_AddStringToObject(state, "effective", effective ? effective : "unknown");
            if (!effective) cJSON_AddStringToObject(state, "reason", "执行器未回报实际隔离状态");
            if (j->kind == JOB_HOST) {
                cJSON *m = msg_new("run_result", NULL);
                cJSON_AddStringToObject(m, "id", j->id);
                cJSON_AddBoolToObject(m, "ok", ok);
                cJSON_AddStringToObject(m, "content", res);
                cJSON_AddItemToObject(m, "sandbox", cJSON_Duplicate(state, 1));
                link_send(&k->link, m);
            } else {
                tool_done_state(k, j->sid, j->run_id, j->turn_id, j->id, ok, res, state, now);
            }
            cJSON_Delete(raw);
            cJSON_Delete(state);
            free(res);
        } else if (j->kind != JOB_HOST) {
            /* 被终止的（超时、停止、断线）也要让状态机闭合这个调用，不能只等它自己超时 */
            tool_done(k, j->sid, j->run_id, j->turn_id, j->id, false, "已终止", now);
        }
        lease_send(k, "lease_release", j->lease_id);
        if (j->tag == 1 && k->nshells > 0) k->nshells--;
        job_free(j);
        pump_execq(k, now);   /* 腾出来的槽位给在排队的调用 */
    }
}

static int64_t next_wakeup(kernel_t *k, int64_t now)
{
    int64_t wake = now + 1000;   /* 至少每秒醒一次，顺带处理退出后迟迟不关管道的进程 */
    int64_t job_deadline=k->jobs?jobs_next_deadline(k->jobs):-1;
    if(job_deadline>=0 && job_deadline<wake)wake=job_deadline;
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
        if (k->leasing && !k->lease_synced && k->gw_ver > 0 && k->link.fd != NA_SOCK_NONE && now >= k->next_lease_sync)
            lease_sync(k, now);
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
        pump_jobs(k,now);
        pump_execq(k,now);

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
    const cJSON *stream=cJSON_GetObjectItemCaseSensitive(r,"job_stream");
    k->exec_stream_version=cJSON_IsNumber(stream)?stream->valueint:0;
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
    unsigned char epoch[16];
    if (plat_random_bytes(epoch, sizeof epoch) < 0) { log_error("无法生成节点启动身份"); return 1; }
    for (size_t i = 0; i < sizeof epoch; i++) snprintf(k.node_epoch + i * 2, 3, "%02x", epoch[i]);

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
    /* 本机的沙箱请求模式：配置里写的（默认 auto），执行节点自己的要求不能被网关绕开 */
    k.sb_mode = !strcmp(k.conf.sandbox_mode, "off") ? SB_OFF
              : !strcmp(k.conf.sandbox_mode, "on") ? SB_ON : SB_AUTO;
    if (k.sb_mode == SB_ON && k.landlock == 0) {
        log_error("kernel.conf 里 sandbox = on，但这台机器没有可用的 Landlock：明确失败，不静默降级。"
                  "改用 sandbox = auto 或 off 再启动");
        free(k.exec_real);
        kconf_free(&k.conf);
        return 1;
    }
    if (k.landlock > 0)
        log_info("执行器 %s，沙箱 Landlock v%d（模式 %s）%s", k.conf.exec, k.landlock, sb_name(k.sb_mode),
                 k.rg ? "" : "（没有 rg，Grep 用 grep 代替）");
    else if (k.sb_mode == SB_OFF)
        log_warn("执行器 %s：sandbox = off，命令按当前 OS 用户身份直接执行，没有 Landlock / seccomp 隔离", k.conf.exec);
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
        /* 本机的并行与排队限额：网关每一轮还可以往下压，但不能往上顶 */
        .max_parallel_tools_per_run = k.conf.max_parallel_tools,
        .max_active_runs = k.conf.max_active_runs,
    };
    k.core = na_core_new(&cc);
    pipes_init(&k.pipes);
    pipes_set_wake(&k.pipes, &g_wake);
    /* 执行器的输出是一行 JSON，内容转义后可能比原文长不少，留足余量 */
    k.runner.out_cap = k.conf.max_output * 6 + 4096;
    k.runner.wake = &g_wake;

    setup_signals();
    char *job_root=ctl_sidecar(conf_path,".jobs");
    k.jobs=jobs_open(job_root,k.node_epoch,&g_wake,k.jobs_error,sizeof k.jobs_error);
    free(job_root);
    if(!k.jobs)log_error("Job 存储恢复失败，后台能力停用并保留已有租约：%s",k.jobs_error);
    else {
        cJSON *records=jobs_list(k.jobs);
        const cJSON *record;
        cJSON_ArrayForEach(record,records) {
            job_binding *b=xcalloc(1,sizeof *b);
            b->job=jobs_find(k.jobs,sget(record,"job_id"));b->recovered=true;b->wait_closed=true;
            b->sid=xstrdup(sget(record,"wait_sid")?sget(record,"wait_sid"):"");
            b->id=xstrdup(sget(record,"wait_id")?sget(record,"wait_id"):"");
            const cJSON *origin=cJSON_GetObjectItemCaseSensitive(record,"origin");
            b->run_id=xstrdup(sget(origin,"run_id")?sget(origin,"run_id"):"");
            const cJSON *turn=cJSON_GetObjectItemCaseSensitive(origin,"turn_id");
            b->turn_id=cJSON_IsNumber(turn)?turn->valueint:0;
            const cJSON *kind=cJSON_GetObjectItemCaseSensitive(record,"wait_kind");
            b->kind=cJSON_IsNumber(kind)?kind->valueint:JOB_HOST;
            const cJSON *spec=cJSON_GetObjectItemCaseSensitive(record,"lease");
            if(cJSON_IsObject(spec)) {
                b->lease_spec=cJSON_PrintUnformatted(spec);
                b->lease_id=xstrdup(sget(spec,"lease_id")?sget(spec,"lease_id"):"");
            }
            b->terminal_handled=!sget(record,"status") || strcmp(sget(record,"status"),"unknown");
            if(!b->terminal_handled)k.nshells++;
            b->next=k.job_bindings;k.job_bindings=b;
        }
        cJSON_Delete(records);
    }
    loop(&k);

    log_info(k.restart ? "重启" : "退出");
    if (!k.restart) {   /* 更新后的重启（exec）还是同一个进程，pid 不变，文件留着 */
        plat_unlink(k.status_file);
        plat_unlink(k.pid_file);
    }
    plat_signals_remove();
    runner_shutdown(&k.runner, 2500);
    /* Shutdown preserves the durable queued records for interrupted recovery. */
    while(k.execq){execq_t *next=k.execq->next;execq_free(k.execq);k.execq=next;}
    pendin_clear(&k);
    while(k.gwjobs){gwjob_t *next=k.gwjobs->next;gw_free(k.gwjobs);k.gwjobs=next;}
    for(job_binding *b=k.job_bindings;b;) {
        job_binding *next=b->next;
        /* Only drained processes removed from runner are retained by the binding. */
        binding_free(b);b=next;
    }
    jobs_close(k.jobs);
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
