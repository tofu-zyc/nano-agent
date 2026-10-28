/* core.c —— 内核状态机实现，见 core.h */
#include "core.h"

#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* 调用的状态。审批和排队都不占执行槽；执行时限从宿主确认 started 起算，
 * 派发到 started 之间另有一个受限的等待。 */
typedef enum {
    CALL_VALIDATED,      /* 刚收到，还没决定怎么处理 */
    CALL_WAIT_APPROVAL,  /* 等管理员审批 */
    CALL_QUEUED,         /* 等执行槽或等资源 */
    CALL_DISPATCHED,     /* 已经交给宿主，等它确认真的开始了 */
    CALL_RUNNING,        /* 宿主确认开始，执行时限在走 */
    CALL_DONE,           /* 已经有终态结果 */
} call_state;

typedef struct {
    char *id, *name, *args, *res_keys, *res_xkeys;
    const na_tool_policy *tool;
    bool elevated;
    bool sent;           /* EXEC 已经发给宿主了：它占着宿主那边的槽位 */
    int  turn_id;        /* 产生这个调用的模型轮次 */
    call_state st;
    int64_t deadline;    /* -1 表示不限（一直等审批 / 一直排队） */
} call_t;

typedef enum { RUN_WAIT_MODEL, RUN_WAIT_TOOLS } run_state;

typedef struct run {
    char *sid, *run_id, *from, *policy;
    na_mode mode;
    bool elevate;        /* 这个身份能不能提权 */
    bool sandbox;        /* 这一轮执行工具的机器有没有沙箱 */
    run_state st;
    int approval_timeout_ms;  /* 不大于 0 表示一直等 */
    int max_steps;            /* 最多请求几次模型，不大于 0 表示不限 */
    int steps;                /* 已请求模型的次数 */
    int turn_id;              /* 当前轮次，从 1 开始 */
    int parallel_limit;       /* 同一 run 同时跑几个工具，不大于 0 不限 */
    int queued_limit;         /* 排队上限，不大于 0 不限 */
    int dispatch_timeout_ms;  /* 派发到 started 的等待上限 */
    int queued_timeout_ms;    /* 排队等待上限，0 表示不单独计时 */
    int64_t deadline;         /* 整个 run 的截止时刻，-1 表示不限 */
    call_t *calls; size_t ncalls;  /* 当前这一轮的工具调用 */
    struct run *next;
} run_t;

struct na_core {
    na_core_config cfg;
    run_t *runs;
    na_cmd *head, *tail;
};

/* 错误信息里引用的参数最多保留这么多字节 */
#define ARGS_PREVIEW 200

/* 没在配置里写时的并行默认值（设计文档 §3.1） */
#define DEF_MAX_CALLS_PER_TURN   32
#define DEF_MAX_PARALLEL         4
#define DEF_MAX_QUEUED           32
#define DEF_DISPATCH_TIMEOUT_MS  30000
#define DEF_MAX_ACTIVE_RUNS      16

static void *xcalloc(size_t n, size_t sz)
{
    void *p = calloc(n ? n : 1, sz);
    if (!p) { fputs("nano-kernel: out of memory\n", stderr); abort(); }
    return p;
}

static char *dupstr(const char *s)
{
    if (!s) return NULL;
    size_t n = strlen(s);
    char *p = xcalloc(1, n + 1);
    memcpy(p, s, n);
    return p;
}

#define PRINTF_LIKE(a, b) __attribute__((format(printf, a, b)))

PRINTF_LIKE(1, 0)
static char *vfmt(const char *f, va_list ap)
{
    char small[256];
    va_list ap2;
    va_copy(ap2, ap);
    int n = vsnprintf(small, sizeof small, f, ap2);
    va_end(ap2);
    if (n < 0) return dupstr("");
    if ((size_t)n < sizeof small) return dupstr(small);
    char *p = xcalloc(1, (size_t)n + 1);
    vsnprintf(p, (size_t)n + 1, f, ap);
    return p;
}

PRINTF_LIKE(1, 2)
static char *fmt(const char *f, ...)
{
    va_list ap;
    va_start(ap, f);
    char *s = vfmt(f, ap);
    va_end(ap);
    return s;
}

/* 截到不超过 max 字节，且不切断 UTF-8 字符 */
static char *preview(const char *s, size_t max)
{
    size_t n = strlen(s);
    if (n <= max) return dupstr(s);
    while (max > 0 && ((unsigned char)s[max] & 0xC0) == 0x80) max--;
    char *p = xcalloc(1, max + 4);
    memcpy(p, s, max);
    memcpy(p + max, "...", 3);
    return p;
}

/* ---- 命令队列 ---- */

static na_cmd *push(na_core *c, na_cmd_type t, const char *sid)
{
    na_cmd *m = xcalloc(1, sizeof *m);
    m->type = t;
    m->sid = dupstr(sid);
    if (c->tail) c->tail->next = m; else c->head = m;
    c->tail = m;
    return m;
}

na_cmd *na_core_next(na_core *c)
{
    na_cmd *m = c->head;
    if (!m) return NULL;
    c->head = m->next;
    if (!c->head) c->tail = NULL;
    m->next = NULL;
    return m;
}

void na_cmd_free(na_cmd *m)
{
    if (!m) return;
    free(m->sid); free(m->id); free(m->name);
    free(m->args); free(m->from); free(m->text); free(m->policy);
    free(m->run_id); free(m->execution);
    free(m);
}

PRINTF_LIKE(3, 4)
static void notice(na_core *c, const char *sid, const char *f, ...)
{
    va_list ap;
    va_start(ap, f);
    push(c, NA_CMD_NOTICE, sid)->text = vfmt(f, ap);
    va_end(ap);
}

/* ---- 查找 ---- */

static run_t *find_run(na_core *c, const char *sid)
{
    if (!sid) return NULL;
    for (run_t *r = c->runs; r; r = r->next)
        if (strcmp(r->sid, sid) == 0) return r;
    return NULL;
}

/* 事件带回来的 run_id 必须和当前 run 一致：跨 run 的迟到消息不能改动当前 run。
 * 空表示旧网关不带，按兼容处理。 */
static bool same_run(const run_t *r, const na_event *ev)
{
    if (!ev->run_id || !*ev->run_id) return true;
    if (!r->run_id || !*r->run_id) return true;
    return strcmp(ev->run_id, r->run_id) == 0;
}

static call_t *find_call(run_t *r, const char *id)
{
    if (!id) return NULL;
    for (size_t i = 0; i < r->ncalls; i++)
        if (strcmp(r->calls[i].id, id) == 0) return &r->calls[i];
    return NULL;
}

static const na_tool_policy *find_tool(na_core *c, const char *name)
{
    for (size_t i = 0; i < c->cfg.ntools; i++)
        if (strcmp(c->cfg.tools[i].name, name) == 0) return &c->cfg.tools[i];
    return NULL;
}

/* 内核不认识的工具名交给网关执行：MCP 工具（<服务>__<工具>）是网关动态带来的，内核没有静态表。
 * 网关认不出来会回一条错误结果，模型能看到。 */
static const na_tool_policy FORWARDED = { "?", NA_TOOL_PUBLIC_READ, 30 * 60 * 1000 };

typedef enum { DECIDE_RUN, DECIDE_ASK, DECIDE_DENY } decision;

/* 按权限模式决定这次调用怎么处理；拒绝时 why 是给模型看的原因 */
static decision decide(bool sandbox, bool sandbox_off, na_mode mode, na_tool_kind kind, bool elevated,
                       bool allowElevate, const char **why)
{
    if (kind == NA_TOOL_PUBLIC_READ) return DECIDE_RUN;
    if (kind == NA_TOOL_ADMIN) {
        if (!allowElevate) {
            *why = "当前身份不能使用网关管理工具";
            return DECIDE_DENY;
        }
        if (mode == NA_MODE_NONE || mode == NA_MODE_READONLY) {
            *why = "当前是只读模式，不能使用网关管理工具";
            return DECIDE_DENY;
        }
        return DECIDE_ASK;   /* 管理工具在任何模式都要审批 */
    }
    if (elevated) {
        if (!allowElevate) {
            *why = "当前身份不能提权写工作目录外";
            return DECIDE_DENY;
        }
        if (mode == NA_MODE_NONE || mode == NA_MODE_READONLY) {
            *why = "当前是只读模式，不能提权写工作目录外";
            return DECIDE_DENY;
        }
        return DECIDE_ASK;   /* auto 也要每次审批 */
    }
    switch (mode) {
    case NA_MODE_NONE:
        *why = "当前身份没有使用工具的权限";
        return DECIDE_DENY;
    case NA_MODE_READONLY:
        if (kind == NA_TOOL_READ) return DECIDE_RUN;
        *why = "当前是只读模式，只能用 Read 和 Grep";
        return DECIDE_DENY;
    case NA_MODE_ASK:
        return kind == NA_TOOL_READ ? DECIDE_RUN : DECIDE_ASK;
    case NA_MODE_AUTO:
        /* 没有沙箱时 Shell 退回审批；但“管理员明确配置了 off”不是意外，按配置直接执行 */
        return kind == NA_TOOL_SHELL && !sandbox && !sandbox_off ? DECIDE_ASK : DECIDE_RUN;
    }
    *why = "未知的权限模式";
    return DECIDE_DENY;
}

/* ---- 资源冲突 ----
 *
 * 宿主按真实工具和已验证参数算出资源键，内核只做集合比较：两个调用只要共有一个
 * 资源键、且其中至少一方是独占的，就不能同时跑。空键表示不和别人抢东西（比如
 * 纯网络查询），两边都是共享锁时可以并行。 */

bool na_resource_overlap(const char *a, size_t na, const char *b, size_t nb)
{
    if (!a || !b || !na || !nb) return false;
    if (na == nb && !memcmp(a, b, na)) return true;
    if (na < 4 || nb < 4 || memcmp(a, "dir:", 4) || memcmp(b, "dir:", 4)) return false;
    if (na > nb) { const char *tmp = a; a = b; b = tmp; size_t n = na; na = nb; nb = n; }
    if (memcmp(a, b, na)) return false;
    return a[na - 1] == '/' || a[na - 1] == '\\' || b[na] == '/' || b[na] == '\\';
}

/* 两组键有没有交集（都是逗号分隔的资源键） */
static bool keys_share(const char *a, const char *b)
{
    if (!a || !*a || !b || !*b) return false;
    for (const char *p = a; *p;) {
        const char *e = strchr(p, ',');
        size_t n = e ? (size_t)(e - p) : strlen(p);
        if (n) {
            for (const char *q = b; *q;) {
                const char *f = strchr(q, ',');
                size_t m = f ? (size_t)(f - q) : strlen(q);
                if (na_resource_overlap(p, n, q, m)) return true;
                q += m + (f ? 1 : 0);
            }
        }
        p += n + (e ? 1 : 0);
    }
    return false;
}

/* 两个调用能不能同时跑：任何一方的独占键和对方的任何键相交就冲突。
 * 文件操作在父目录上取共享锁、Shell 在目录上取独占锁，于是“Shell vs 同目录的写”
 * 一定会冲突，而“不同文件的写”可以并行。 */
static bool calls_conflict(const call_t *a, const call_t *b)
{
    if (a->res_xkeys && *a->res_xkeys &&
        (keys_share(a->res_xkeys, b->res_xkeys) || keys_share(a->res_xkeys, b->res_keys))) return true;
    if (b->res_xkeys && *b->res_xkeys && keys_share(b->res_xkeys, a->res_keys)) return true;
    return false;
}

/* Dispatch handshakes reserve a bounded slot until the host acknowledges.
 * An acknowledged queued call owns no execution slot; the host/Gateway arbiter
 * enforces the real per-run/machine limits before reporting CALL_STARTED. */
static bool call_holds_slot(const call_t *k)
{
    return k->sent && (k->st == CALL_DISPATCHED || k->st == CALL_RUNNING);
}

static int run_slots_used(const run_t *r)
{
    int n = 0;
    for (size_t i = 0; i < r->ncalls; i++)
        if (call_holds_slot(&r->calls[i])) n++;
    return n;
}

static int run_queued(const run_t *r)
{
    int n = 0;
    for (size_t i = 0; i < r->ncalls; i++)
        if (r->calls[i].st == CALL_QUEUED) n++;
    return n;
}

static bool has_free_slot(const run_t *r)
{
    if (r->parallel_limit <= 0) return true;
    return run_slots_used(r) < r->parallel_limit;
}

/* 和其他已经占了槽位的调用有没有资源冲突 */
static bool can_start(const run_t *r, const call_t *k)
{
    for (size_t i = 0; i < r->ncalls; i++) {
        const call_t *o = &r->calls[i];
        if (o == k || !call_holds_slot(o)) continue;
        if (calls_conflict(o, k)) return false;
    }
    return true;
}

/* ---- run 生命周期 ---- */

static void free_calls(run_t *r)
{
    for (size_t i = 0; i < r->ncalls; i++) {
        free(r->calls[i].id); free(r->calls[i].name); free(r->calls[i].args);
        free(r->calls[i].res_keys); free(r->calls[i].res_xkeys);
    }
    free(r->calls);
    r->calls = NULL;
    r->ncalls = 0;
}

static void drop_run(na_core *c, run_t *r)
{
    for (run_t **pp = &c->runs; *pp; pp = &(*pp)->next) {
        if (*pp == r) { *pp = r->next; break; }
    }
    free_calls(r);
    free(r->sid); free(r->run_id); free(r->from); free(r->policy);
    free(r);
}

static void finish(na_core *c, run_t *r, const char *status, const char *detail)
{
    na_cmd *m = push(c, NA_CMD_DONE, r->sid);
    m->run_id = dupstr(r->run_id);
    m->status = status;
    m->text = dupstr(detail);
    drop_run(c, r);
}

/* 请求下一轮模型；超出步数预算则结束 run */
static void step(na_core *c, run_t *r)
{
    free_calls(r);
    if (r->max_steps > 0 && r->steps >= r->max_steps) {
        finish(c, r, "budget", NULL);
        return;
    }
    r->steps++;
    r->turn_id++;
    r->st = RUN_WAIT_MODEL;
    na_cmd *m = push(c, NA_CMD_MODEL, r->sid);
    m->run_id = dupstr(r->run_id);
    m->turn_id = r->turn_id;
}

static na_cmd *result(na_core *c, run_t *r, call_t *k, bool ok, const char *text)
{
    na_cmd *m = push(c, NA_CMD_RESULT, r->sid);
    m->id = dupstr(k->id);
    m->run_id = dupstr(r->run_id);
    m->turn_id = k->turn_id;
    m->ok = ok;
    m->text = dupstr(text);
    k->st = CALL_DONE;
    k->deadline = -1;
    return m;
}

PRINTF_LIKE(4, 5)
static void result_err(na_core *c, run_t *r, call_t *k, const char *f, ...)
{
    va_list ap;
    va_start(ap, f);
    char *text = vfmt(f, ap);
    va_end(ap);
    result(c, r, k, false, text);
    free(text);
}

static void kill_call(na_core *c, run_t *r, call_t *k)
{
    na_cmd *m = push(c, NA_CMD_KILL, r->sid);
    m->id = dupstr(k->id);
    m->run_id = dupstr(r->run_id);
    m->turn_id = k->turn_id;
}

/* 把调用交给宿主。执行时限不在这里开始：等宿主回 CALL_STARTED 才算真的开始了。 */
static void exec_call(na_core *c, int64_t now, run_t *r, call_t *k)
{
    na_cmd *m = push(c, NA_CMD_EXEC, r->sid);
    m->id = dupstr(k->id);
    m->name = dupstr(k->name);
    m->args = dupstr(k->args);
    m->from = dupstr(r->from);
    m->policy = dupstr(r->policy);
    m->run_id = dupstr(r->run_id);
    m->turn_id = k->turn_id;
    m->timeout_ms = k->tool->timeout_ms;
    m->elevated = k->elevated;
    k->st = CALL_DISPATCHED;
    k->sent = true;
    k->deadline = r->dispatch_timeout_ms > 0 ? now + r->dispatch_timeout_ms : -1;
}

static void queued_notice(na_core *c, run_t *r, call_t *k, const char *why)
{
    na_cmd *m = push(c, NA_CMD_QUEUED, r->sid);
    m->id = dupstr(k->id);
    m->name = dupstr(k->name);
    m->run_id = dupstr(r->run_id);
    m->turn_id = k->turn_id;
    m->text = dupstr(why);
}

/* 按顺序把能开始的调用放到执行槽里。每次只有一个调用结束时也要调一次。 */
static void dispatch(na_core *c, int64_t now, run_t *r)
{
    for (;;) {
        if (!has_free_slot(r)) return;
        call_t *pick = NULL;
        for (size_t i = 0; i < r->ncalls; i++) {
            call_t *k = &r->calls[i];
            if (k->st != CALL_QUEUED) continue;
            /* 已经发给宿主的（宿主那边在排队等槽位/资源）不能再派发一次：
             * 它已经有人管了，重复 EXEC 会让同一次调用真的执行好几遍。 */
            if (k->sent) continue;
            if (!can_start(r, k)) continue;
            pick = k;
            break;   /* 从最早排队的开始挑，避免后来的插队饿死前面的 */
        }
        if (!pick) return;
        exec_call(c, now, r, pick);
    }
}

/* 一轮的工具调用全部有了结果，就请求下一轮模型 */
static void maybe_advance(na_core *c, run_t *r)
{
    if (r->st != RUN_WAIT_TOOLS) return;
    for (size_t i = 0; i < r->ncalls; i++)
        if (r->calls[i].st != CALL_DONE) return;
    step(c, r);
}

/* 给所有还没有结果的调用补上结果，正在执行（或已派发）的先终止 */
static void abort_calls(na_core *c, run_t *r, const char *why)
{
    for (size_t i = 0; i < r->ncalls; i++) {
        call_t *k = &r->calls[i];
        if (k->st == CALL_DONE) continue;
        if (k->sent) kill_call(c, r, k);
        result(c, r, k, false, why);
    }
}

/* ---- 事件处理 ---- */

static int on_input(na_core *c, int64_t now, const na_event *ev)
{
    if (!ev->sid || find_run(c, ev->sid)) return -1;
    run_t *r = xcalloc(1, sizeof *r);
    r->sid = dupstr(ev->sid);
    r->run_id = dupstr(ev->run_id ? ev->run_id : "");
    r->from = dupstr(ev->from ? ev->from : "");
    r->policy = dupstr(ev->policy ? ev->policy : "{}");
    r->mode = ev->mode;
    r->elevate = ev->elevate;
    r->sandbox = ev->sandbox == NA_SB_DEFAULT ? c->cfg.sandbox : ev->sandbox == NA_SB_YES;
    /* 网关每一轮带上自己的设置（大于 0 是上限，小于 0 不限）；没带（旧网关）用内核配置 */
    int run_ms = ev->run_timeout_ms ? ev->run_timeout_ms : c->cfg.run_timeout_ms;
    r->deadline = run_ms > 0 ? now + run_ms : -1;
    r->approval_timeout_ms = ev->approval_timeout_ms ? ev->approval_timeout_ms : c->cfg.approval_timeout_ms;
    r->max_steps = ev->max_steps ? ev->max_steps : c->cfg.max_steps;
    r->parallel_limit = ev->max_parallel_tools > 0 ? ev->max_parallel_tools
                       : c->cfg.max_parallel_tools_per_run > 0 ? c->cfg.max_parallel_tools_per_run : DEF_MAX_PARALLEL;
    if (c->cfg.max_parallel_tools_per_run > 0 && r->parallel_limit > c->cfg.max_parallel_tools_per_run)
        r->parallel_limit = c->cfg.max_parallel_tools_per_run;
    r->queued_limit = c->cfg.max_queued_calls_per_run > 0 ? c->cfg.max_queued_calls_per_run : DEF_MAX_QUEUED;
    r->dispatch_timeout_ms = c->cfg.dispatch_timeout_ms > 0 ? c->cfg.dispatch_timeout_ms : DEF_DISPATCH_TIMEOUT_MS;
    r->queued_timeout_ms = c->cfg.queued_timeout_ms;
    r->next = c->runs;
    c->runs = r;
    step(c, r);
    return 0;
}

/* 一批调用在结构上是否自洽：数量、空 ID、重复 ID。任何一条不满足，整轮按模型协议
 * 错误处理——不能执行其中一部分再报错，那样工具结果的配对就没法保证了。 */
static char *turn_badness(na_core *c, const na_event *ev)
{
    int lim = c->cfg.max_calls_per_turn > 0 ? c->cfg.max_calls_per_turn : DEF_MAX_CALLS_PER_TURN;
    if ((int)ev->ncalls > lim)
        return fmt("这一轮有 %zu 个工具调用，超过了上限 %d。请分成几轮来做", ev->ncalls, lim);
    for (size_t i = 0; i < ev->ncalls; i++) {
        const char *id = ev->calls[i].id;
        if (!id || !*id) return fmt("第 %zu 个工具调用没有 id，无法配对结果", i + 1);
        for (size_t j = 0; j < i; j++)
            if (ev->calls[j].id && !strcmp(ev->calls[j].id, id))
                return fmt("这一轮里有两个调用用了同一个 id（%s），无法可靠配对结果", id);
    }
    return NULL;
}

static int on_turn(na_core *c, int64_t now, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    if (!r || r->st != RUN_WAIT_MODEL || !same_run(r, ev)) return -1;
    if (ev->turn_id > 0 && ev->turn_id != r->turn_id) return -1;   /* 上一轮的迟到回复 */

    switch (ev->reason) {
    case NA_TURN_FINAL:   finish(c, r, "ok", NULL);            return 0;
    case NA_TURN_REFUSED: finish(c, r, "refused", ev->detail); return 0;
    case NA_TURN_ERROR:   finish(c, r, "error", ev->detail);   return 0;
    case NA_TURN_TOOL_USE: break;
    }
    if (ev->ncalls == 0) {
        finish(c, r, "ok", NULL);
        return 0;
    }
    char *bad = turn_badness(c, ev);
    if (bad) {
        finish(c, r, "error", bad);
        free(bad);
        return 0;
    }

    r->calls = xcalloc(ev->ncalls, sizeof *r->calls);
    r->ncalls = ev->ncalls;
    r->st = RUN_WAIT_TOOLS;
    for (size_t i = 0; i < ev->ncalls; i++) {
        const na_call *in = &ev->calls[i];
        call_t *k = &r->calls[i];
        k->id = dupstr(in->id ? in->id : "");
        k->name = dupstr(in->name ? in->name : "");
        k->args = dupstr(in->args ? in->args : "{}");
        k->res_keys = dupstr(in->res_keys ? in->res_keys : "");
        k->res_xkeys = dupstr(in->res_xkeys ? in->res_xkeys : "");
        k->turn_id = r->turn_id;
        k->tool = find_tool(c, k->name);
        /* 内核不认识的工具名（MCP 的 <服务>__<工具>）交给网关执行 */
        if (!k->tool) k->tool = &FORWARDED;
        const char *why = NULL;
        na_sandbox run_sb = in->sandbox == NA_SB_DEFAULT ? (r->sandbox ? NA_SB_YES : NA_SB_NO) : in->sandbox;
        bool sandbox = run_sb == NA_SB_YES || run_sb == NA_SB_OFF;   /* OFF 也算“隔离状态已经明确” */
        bool sandbox_off = run_sb == NA_SB_OFF;
        k->elevated = in->elevated;
        na_tool_kind kind = k->tool ? k->tool->kind : NA_TOOL_WRITE;
        bool schedule = kind == NA_TOOL_SCHEDULE;
        bool history_delete = kind == NA_TOOL_ADMIN && in->action && !strcmp(in->action, "history_delete");
        /* Schedule：查看和立即运行按只读算，增删改按写算 */
        if (schedule)
            kind = in->action && (!strcmp(in->action, "list") || !strcmp(in->action, "run_now")) ? NA_TOOL_READ : NA_TOOL_WRITE;
        decision d = k->tool ? decide(sandbox, sandbox_off, r->mode, kind, k->elevated, r->elevate, &why) : DECIDE_DENY;
        if (schedule && !r->elevate) {
            d = DECIDE_DENY;
            why = "只有管理员能管理定时任务";
        }
        if (history_delete && r->elevate)
            d = DECIDE_ASK; /* 删除历史在所有模式（包括只读）都要审批 */

        if (!in->args_ok) {
            char *p = preview(k->args, ARGS_PREVIEW);
            result_err(c, r, k, "参数不是合法的 JSON 对象：%s", p);
            free(p);
        } else if (in->error) {
            result(c, r, k, false, in->error);
        } else if (d == DECIDE_DENY) {
            result(c, r, k, false, why);
        } else if (d == DECIDE_ASK) {
            na_cmd *m = push(c, NA_CMD_APPROVE_REQ, r->sid);
            m->id = dupstr(k->id);
            m->name = dupstr(k->name);
            m->args = dupstr(k->args);
            m->from = dupstr(r->from);
            m->run_id = dupstr(r->run_id);
            m->turn_id = k->turn_id;
            k->st = CALL_WAIT_APPROVAL;
            k->deadline = r->approval_timeout_ms > 0 ? now + r->approval_timeout_ms : -1;
        } else {
            if (r->queued_limit > 0 && run_queued(r) >= r->queued_limit) {
                result_err(c, r, k, "这一轮排队的工具调用已经有 %d 个（上限），这个没有执行。请分几轮来做", r->queued_limit);
                continue;
            }
            k->st = CALL_QUEUED;
            k->deadline = r->queued_timeout_ms > 0 ? now + r->queued_timeout_ms : -1;
            if (!has_free_slot(r) || !can_start(r, k))
                queued_notice(c, r, k, "等前面的工具执行完");
            dispatch(c, now, r);
        }
    }
    maybe_advance(c, r);
    return 0;
}

static int on_approve(na_core *c, int64_t now, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    if (r && !same_run(r, ev)) return -1;
    call_t *k = r ? find_call(r, ev->id) : NULL;
    if (!k || k->st != CALL_WAIT_APPROVAL) return -1;
    if (ev->turn_id > 0 && k->turn_id && ev->turn_id != k->turn_id) return -1;

    if (ev->ok) {
        notice(c, r->sid, "已批准，开始执行 %s", k->name);
        if (r->queued_limit > 0 && run_queued(r) >= r->queued_limit) {
            result_err(c, r, k, "这一轮排队的工具调用已经有 %d 个（上限），这个没有执行。请分几轮来做", r->queued_limit);
            maybe_advance(c, r);
            return 0;
        }
        k->st = CALL_QUEUED;
        k->deadline = r->queued_timeout_ms > 0 ? now + r->queued_timeout_ms : -1;
        if (!has_free_slot(r) || !can_start(r, k))
            queued_notice(c, r, k, "等前面的工具执行完");
        dispatch(c, now, r);
    } else {
        result(c, r, k, false, "管理员拒绝执行");
        dispatch(c, now, r);
        maybe_advance(c, r);
    }
    return 0;
}

/* 宿主收到了，但在等执行槽或等资源：从“派发等待”转成“排队”，
 * 执行时限和派发等待都不再走，改由排队时限（没配就只受 run 总时限约束）。 */
static int on_call_queued(na_core *c, int64_t now, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    if (r && !same_run(r, ev)) return -1;
    call_t *k = r ? find_call(r, ev->id) : NULL;
    if (!k || k->st != CALL_DISPATCHED) return -1;
    if (ev->turn_id > 0 && k->turn_id && ev->turn_id != k->turn_id) return -1;
    k->st = CALL_QUEUED;
    k->deadline = r->queued_timeout_ms > 0 ? now + r->queued_timeout_ms : -1;
    dispatch(c, now, r);
    return 0;
}

static int on_call_started(na_core *c, int64_t now, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    if (r && !same_run(r, ev)) return -1;
    call_t *k = r ? find_call(r, ev->id) : NULL;
    /* 直接从派发等待进来，或者先被宿主排过队 */
    if (!k || !k->sent || (k->st != CALL_DISPATCHED && k->st != CALL_QUEUED)) return -1;
    if (ev->turn_id > 0 && k->turn_id && ev->turn_id != k->turn_id) return -1;
    k->st = CALL_RUNNING;
    k->deadline = k->tool->timeout_ms > 0 ? now + k->tool->timeout_ms : -1;
    return 0;
}

/* 网关说“这次生成让位给插话”：run 还停在等模型，按原来的步数预算再请求一次。
 * 预算是内核的：插话不会重置计数，也不能靠反复插话绕过上限。 */
static int on_model_yield(na_core *c, int64_t now, const na_event *ev)
{
    (void)now;
    run_t *r = find_run(c, ev->sid);
    if (!r || r->st != RUN_WAIT_MODEL || !same_run(r, ev)) return -1;
    if (ev->turn_id > 0 && r->turn_id && ev->turn_id != r->turn_id) return -1;
    if (r->max_steps > 0 && r->steps >= r->max_steps) {
        finish(c, r, "budget", NULL);
        return 0;
    }
    r->steps++;
    r->turn_id++;
    na_cmd *m = push(c, NA_CMD_MODEL, r->sid);
    m->run_id = dupstr(r->run_id);
    m->turn_id = r->turn_id;
    return 0;
}

static int on_cancel(na_core *c, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    if (!r) return -1;
    abort_calls(c, r, "已取消");
    finish(c, r, "cancelled", NULL);
    return 0;
}

static int on_tool_done(na_core *c, int64_t now, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    if (r && !same_run(r, ev)) return -1;
    call_t *k = r ? find_call(r, ev->id) : NULL;
    /* 超时或取消之后才到的结果；跨轮次、跨 run 的迟到结果也在这里被挡住 */
    if (!k || (k->st != CALL_RUNNING && k->st != CALL_DISPATCHED &&
               !(k->st == CALL_QUEUED && k->sent))) return -1;
    if (ev->turn_id > 0 && k->turn_id && ev->turn_id != k->turn_id) return -1;
    na_cmd *reply=result(c, r, k, ev->ok, ev->content ? ev->content : "");
    reply->execution=dupstr(ev->execution);
    dispatch(c, now, r);
    maybe_advance(c, r);
    return 0;
}

static void on_tick(na_core *c, int64_t now)
{
    run_t *next;
    for (run_t *r = c->runs; r; r = next) {
        next = r->next;   /* r 可能在下面被释放 */
        if (r->deadline >= 0 && now >= r->deadline) {
            abort_calls(c, r, "运行超时，已终止");
            finish(c, r, "timeout", NULL);
            continue;
        }
        if (r->st != RUN_WAIT_TOOLS) continue;
        for (size_t i = 0; i < r->ncalls; i++) {
            call_t *k = &r->calls[i];
            if (k->deadline < 0 || now < k->deadline) continue;
            if (k->st == CALL_RUNNING) {
                kill_call(c, r, k);
                result_err(c, r, k, "执行超时（%d 秒），已终止", k->tool->timeout_ms / 1000);
            } else if (k->st == CALL_DISPATCHED) {
                kill_call(c, r, k);
                result(c, r, k, false, "派发超时：执行节点一直没能开始这个调用，已取消");
            } else if (k->st == CALL_WAIT_APPROVAL) {
                result(c, r, k, false, "审批超时，未执行");
            } else if (k->st == CALL_QUEUED) {
                result(c, r, k, false, "排队超时：一直没等到执行槽，未执行");
            }
        }
        dispatch(c, now, r);
        maybe_advance(c, r);
    }
}

static void on_reset(na_core *c)
{
    while (c->runs) {
        run_t *r = c->runs;
        for (size_t i = 0; i < r->ncalls; i++)
            if (r->calls[i].sent) kill_call(c, r, &r->calls[i]);
        drop_run(c, r);
    }
}

int na_core_feed(na_core *c, int64_t now, const na_event *ev)
{
    switch (ev->type) {
    case NA_EV_INPUT:        return on_input(c, now, ev);
    case NA_EV_TURN:         return on_turn(c, now, ev);
    case NA_EV_APPROVE:      return on_approve(c, now, ev);
    case NA_EV_CANCEL:       return on_cancel(c, ev);
    case NA_EV_MODEL_YIELD:  return on_model_yield(c, now, ev);
    case NA_EV_CALL_QUEUED:  return on_call_queued(c, now, ev);
    case NA_EV_CALL_STARTED: return on_call_started(c, now, ev);
    case NA_EV_TOOL_DONE:    return on_tool_done(c, now, ev);
    case NA_EV_TICK:         on_tick(c, now); return 0;
    case NA_EV_RESET:        on_reset(c);     return 0;
    }
    return -1;
}

/* ---- 其他 ---- */

na_core *na_core_new(const na_core_config *cfg)
{
    na_core *c = xcalloc(1, sizeof *c);
    c->cfg = *cfg;
    return c;
}

void na_core_free(na_core *c)
{
    if (!c) return;
    while (c->runs) drop_run(c, c->runs);
    na_cmd *m;
    while ((m = na_core_next(c))) na_cmd_free(m);
    free(c);
}

int64_t na_core_next_deadline(const na_core *c)
{
    int64_t best = -1;
    for (const run_t *r = c->runs; r; r = r->next) {
        if (r->deadline >= 0 && (best < 0 || r->deadline < best)) best = r->deadline;
        for (size_t i = 0; i < r->ncalls; i++) {
            const call_t *k = &r->calls[i];
            if (k->st == CALL_DONE || k->deadline < 0) continue;
            if (best < 0 || k->deadline < best) best = k->deadline;
        }
    }
    return best;
}

size_t na_core_active_runs(const na_core *c)
{
    size_t n = 0;
    for (const run_t *r = c->runs; r; r = r->next) n++;
    return n;
}

int na_core_max_runs(const na_core *c)
{
    return c->cfg.max_active_runs > 0 ? c->cfg.max_active_runs : DEF_MAX_ACTIVE_RUNS;
}

const char *na_core_policy(const na_core *c, const char *sid)
{
    for (const run_t *r = c->runs; r; r = r->next)
        if (sid && !strcmp(r->sid, sid)) return r->policy;
    return NULL;
}

void na_core_call_counts(const na_core *c, const char *sid, int *running, int *queued)
{
    int run_n = 0, queued_n = 0;
    for (const run_t *r = c->runs; r; r = r->next) {
        if (sid && strcmp(r->sid, sid)) continue;
        run_n += run_slots_used(r);
        queued_n += run_queued(r);
    }
    if (running) *running = run_n;
    if (queued) *queued = queued_n;
}
