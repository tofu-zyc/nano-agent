/* core.c —— 内核状态机实现，见 core.h */
#include "core.h"

#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef enum { CALL_WAIT_APPROVAL, CALL_RUNNING, CALL_DONE } call_state;

typedef struct {
    char *id, *name, *args;
    const na_tool_policy *tool;
    bool elevated;
    call_state st;
    int64_t deadline;
} call_t;

typedef enum { RUN_WAIT_MODEL, RUN_WAIT_TOOLS } run_state;

typedef struct run {
    char *sid, *from, *policy;
    na_mode mode;
    bool elevate;        /* 这个身份能不能提权 */
    bool sandbox;        /* 这一轮执行工具的机器有没有沙箱 */
    run_state st;
    int approval_timeout_ms;
    int steps;            /* 已请求模型的次数 */
    int64_t deadline;     /* 整个 run 的截止时刻 */
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

typedef enum { DECIDE_RUN, DECIDE_ASK, DECIDE_DENY } decision;

/* 按权限模式决定这次调用怎么处理；拒绝时 why 是给模型看的原因 */
static decision decide(bool sandbox, na_mode mode, na_tool_kind kind, bool elevated, bool allowElevate, const char **why)
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
        return kind == NA_TOOL_SHELL && !sandbox ? DECIDE_ASK : DECIDE_RUN;
    }
    *why = "未知的权限模式";
    return DECIDE_DENY;
}

/* ---- run 生命周期 ---- */

static void free_calls(run_t *r)
{
    for (size_t i = 0; i < r->ncalls; i++) {
        free(r->calls[i].id); free(r->calls[i].name); free(r->calls[i].args);
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
    free(r->sid); free(r->from); free(r->policy);
    free(r);
}

static void finish(na_core *c, run_t *r, const char *status, const char *detail)
{
    na_cmd *m = push(c, NA_CMD_DONE, r->sid);
    m->status = status;
    m->text = dupstr(detail);
    drop_run(c, r);
}

/* 请求下一轮模型；超出步数预算则结束 run */
static void step(na_core *c, run_t *r)
{
    free_calls(r);
    if (r->steps >= c->cfg.max_steps) {
        finish(c, r, "budget", NULL);
        return;
    }
    r->steps++;
    r->st = RUN_WAIT_MODEL;
    push(c, NA_CMD_MODEL, r->sid);
}

static void result(na_core *c, run_t *r, call_t *k, bool ok, const char *text)
{
    na_cmd *m = push(c, NA_CMD_RESULT, r->sid);
    m->id = dupstr(k->id);
    m->ok = ok;
    m->text = dupstr(text);
    k->st = CALL_DONE;
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

static void exec_call(na_core *c, int64_t now, run_t *r, call_t *k)
{
    na_cmd *m = push(c, NA_CMD_EXEC, r->sid);
    m->id = dupstr(k->id);
    m->name = dupstr(k->name);
    m->args = dupstr(k->args);
    m->from = dupstr(r->from);
    m->policy = dupstr(r->policy);
    m->timeout_ms = k->tool->timeout_ms;
    m->elevated = k->elevated;
    k->st = CALL_RUNNING;
    k->deadline = now + k->tool->timeout_ms;
}

static void kill_call(na_core *c, run_t *r, call_t *k)
{
    push(c, NA_CMD_KILL, r->sid)->id = dupstr(k->id);
}

/* 一轮的工具调用全部有了结果，就请求下一轮模型 */
static void maybe_advance(na_core *c, run_t *r)
{
    if (r->st != RUN_WAIT_TOOLS) return;
    for (size_t i = 0; i < r->ncalls; i++)
        if (r->calls[i].st != CALL_DONE) return;
    step(c, r);
}

/* 给所有还没有结果的调用补上结果，正在执行的先终止 */
static void abort_calls(na_core *c, run_t *r, const char *why)
{
    for (size_t i = 0; i < r->ncalls; i++) {
        call_t *k = &r->calls[i];
        if (k->st == CALL_DONE) continue;
        if (k->st == CALL_RUNNING) kill_call(c, r, k);
        result(c, r, k, false, why);
    }
}

/* ---- 事件处理 ---- */

static int on_input(na_core *c, int64_t now, const na_event *ev)
{
    if (!ev->sid || find_run(c, ev->sid)) return -1;
    run_t *r = xcalloc(1, sizeof *r);
    r->sid = dupstr(ev->sid);
    r->from = dupstr(ev->from ? ev->from : "");
    r->policy = dupstr(ev->policy ? ev->policy : "{}");
    r->mode = ev->mode;
    r->elevate = ev->elevate;
    r->sandbox = ev->sandbox == NA_SB_DEFAULT ? c->cfg.sandbox : ev->sandbox == NA_SB_YES;
    r->deadline = now + (ev->run_timeout_ms>0 ? ev->run_timeout_ms : c->cfg.run_timeout_ms);
 r->approval_timeout_ms=ev->approval_timeout_ms>0 ? ev->approval_timeout_ms : c->cfg.approval_timeout_ms;
    r->next = c->runs;
    c->runs = r;
    step(c, r);
    return 0;
}

static int on_turn(na_core *c, int64_t now, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    if (!r || r->st != RUN_WAIT_MODEL) return -1;

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

    r->calls = xcalloc(ev->ncalls, sizeof *r->calls);
    r->ncalls = ev->ncalls;
    r->st = RUN_WAIT_TOOLS;
    for (size_t i = 0; i < ev->ncalls; i++) {
        const na_call *in = &ev->calls[i];
        call_t *k = &r->calls[i];
        k->id = dupstr(in->id ? in->id : "");
        k->name = dupstr(in->name ? in->name : "");
        k->args = dupstr(in->args ? in->args : "{}");
        k->tool = find_tool(c, k->name);
        const char *why = NULL;
        bool sandbox = in->sandbox == NA_SB_DEFAULT ? r->sandbox : in->sandbox == NA_SB_YES;
        k->elevated = in->elevated;
        na_tool_kind kind=k->tool ? k->tool->kind : NA_TOOL_WRITE;
        bool schedule_admin=kind==NA_TOOL_SCHEDULE;
        bool history_delete=kind==NA_TOOL_ADMIN && in->action && !strcmp(in->action,"history_delete");
        if (schedule_admin) kind=in->action && (!strcmp(in->action,"list") || !strcmp(in->action,"run_now")) ? NA_TOOL_READ : NA_TOOL_WRITE;
        decision d = k->tool ? decide(sandbox, r->mode, kind, k->elevated, r->elevate, &why) : DECIDE_DENY;
        if (schedule_admin && !r->elevate) {d=DECIDE_DENY;why="只有管理员能管理定时任务";}
        if (history_delete && r->elevate) d=DECIDE_ASK; /* 删除历史在所有模式都必须审批 */

        if (!k->tool) {
            result_err(c, r, k, "未知工具：%s", k->name);
        } else if (!in->args_ok) {
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
            k->st = CALL_WAIT_APPROVAL;
            k->deadline = now + r->approval_timeout_ms;
        } else {
            exec_call(c, now, r, k);
        }
    }
    maybe_advance(c, r);
    return 0;
}

static int on_approve(na_core *c, int64_t now, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    call_t *k = r ? find_call(r, ev->id) : NULL;
    if (!k || k->st != CALL_WAIT_APPROVAL) return -1;

    if (ev->ok) {
        notice(c, r->sid, "已批准，开始执行 %s", k->name);
        exec_call(c, now, r, k);
    } else {
        result(c, r, k, false, "管理员拒绝执行");
        maybe_advance(c, r);
    }
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

static int on_tool_done(na_core *c, const na_event *ev)
{
    run_t *r = find_run(c, ev->sid);
    call_t *k = r ? find_call(r, ev->id) : NULL;
    if (!k || k->st != CALL_RUNNING) return -1;   /* 超时或取消之后才到的结果 */
    result(c, r, k, ev->ok, ev->content ? ev->content : "");
    maybe_advance(c, r);
    return 0;
}

static void on_tick(na_core *c, int64_t now)
{
    run_t *next;
    for (run_t *r = c->runs; r; r = next) {
        next = r->next;   /* r 可能在下面被释放 */
        if (now >= r->deadline) {
            abort_calls(c, r, "运行超时，已终止");
            finish(c, r, "timeout", NULL);
            continue;
        }
        if (r->st != RUN_WAIT_TOOLS) continue;
        for (size_t i = 0; i < r->ncalls; i++) {
            call_t *k = &r->calls[i];
            if (now < k->deadline) continue;
            if (k->st == CALL_RUNNING) {
                kill_call(c, r, k);
                result_err(c, r, k, "执行超时（%d 秒），已终止", k->tool->timeout_ms / 1000);
            } else if (k->st == CALL_WAIT_APPROVAL) {
                result(c, r, k, false, "审批超时，未执行");
            }
        }
        maybe_advance(c, r);
    }
}

static void on_reset(na_core *c)
{
    while (c->runs) {
        run_t *r = c->runs;
        for (size_t i = 0; i < r->ncalls; i++)
            if (r->calls[i].st == CALL_RUNNING) kill_call(c, r, &r->calls[i]);
        drop_run(c, r);
    }
}

int na_core_feed(na_core *c, int64_t now, const na_event *ev)
{
    switch (ev->type) {
    case NA_EV_INPUT:     return on_input(c, now, ev);
    case NA_EV_TURN:      return on_turn(c, now, ev);
    case NA_EV_APPROVE:   return on_approve(c, now, ev);
    case NA_EV_CANCEL:    return on_cancel(c, ev);
    case NA_EV_TOOL_DONE: return on_tool_done(c, ev);
    case NA_EV_TICK:      on_tick(c, now); return 0;
    case NA_EV_RESET:     on_reset(c);     return 0;
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
        if (best < 0 || r->deadline < best) best = r->deadline;
        for (size_t i = 0; i < r->ncalls; i++) {
            const call_t *k = &r->calls[i];
            if (k->st == CALL_DONE) continue;
            if (k->deadline < best) best = k->deadline;
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

const char *na_core_policy(const na_core *c, const char *sid)
{
    for (const run_t *r = c->runs; r; r = r->next)
        if (sid && !strcmp(r->sid, sid)) return r->policy;
    return NULL;
}
