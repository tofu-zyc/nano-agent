/* test_core.c —— 状态机单元测试：喂事件序列，断言命令序列 */
#include "../src/core.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int failures, checks;

#define CHECK(cond) do { \
    checks++; \
    if (!(cond)) { failures++; fprintf(stderr, "%s:%d: CHECK failed: %s\n", __FILE__, __LINE__, #cond); } \
} while (0)

static na_tool_policy tools[] = {
    { "Read",  NA_TOOL_READ,  5000 },
    { "Write", NA_TOOL_WRITE, 10000 },
    { "Shell", NA_TOOL_SHELL, 30000 },
    { "GatewayAdmin", NA_TOOL_ADMIN, 610000 },
    { "WebSearch", NA_TOOL_PUBLIC_READ, 70000 },
 {"Schedule",NA_TOOL_SCHEDULE,70000},
};

static na_core *mk_sb(int max_steps, bool sandbox)
{
    na_core_config cfg = {
        .tools = tools, .ntools = 6,
        .sandbox = sandbox,
        .max_steps = max_steps,
        .run_timeout_ms = 600000,
        .approval_timeout_ms = 120000,
    };
    return na_core_new(&cfg);
}

static na_core *mk(int max_steps) { return mk_sb(max_steps, true); }

/* 把取出的命令格式化成一行，便于整体断言 */
static char out[8192];
static const char *drain(na_core *c)
{
    out[0] = 0;
    na_cmd *m;
    while ((m = na_core_next(c))) {
        char item[1024];
        switch (m->type) {
        case NA_CMD_MODEL:       snprintf(item, sizeof item, "MODEL(%s)", m->sid); break;
        case NA_CMD_RESULT:      snprintf(item, sizeof item, "RESULT(%s,%s,%s)", m->id, m->ok ? "ok" : "err", m->text); break;
        case NA_CMD_APPROVE_REQ: snprintf(item, sizeof item, "APPROVE_REQ(%s,%s,%s)", m->id, m->name, m->from); break;
        case NA_CMD_NOTICE:      snprintf(item, sizeof item, "NOTICE(%s)", m->text); break;
        case NA_CMD_DONE:        snprintf(item, sizeof item, "DONE(%s)", m->status); break;
        case NA_CMD_EXEC:        snprintf(item, sizeof item, "EXEC(%s,%s,%d)", m->id, m->name, m->timeout_ms); break;
        case NA_CMD_KILL:        snprintf(item, sizeof item, "KILL(%s)", m->id); break;
        }
        if (out[0]) strncat(out, " ", sizeof out - strlen(out) - 1);
        strncat(out, item, sizeof out - strlen(out) - 1);
        na_cmd_free(m);
    }
    return out;
}

#define EXPECT(c, want) do { \
    const char *got_ = drain(c); \
    checks++; \
    if (strcmp(got_, (want)) != 0) { \
        failures++; \
        fprintf(stderr, "%s:%d:\n  want: %s\n  got:  %s\n", __FILE__, __LINE__, (want), got_); \
    } \
} while (0)

static int input_mode(na_core *c, int64_t now, const char *sid, const char *from, na_mode mode)
{
    na_event ev = { .type = NA_EV_INPUT, .sid = sid, .from = from, .mode = mode, .policy = "{\"dirs\":[\"/w\"]}" };
    return na_core_feed(c, now, &ev);
}

/* 默认用审批模式：只读工具直接执行，其余要审批 */
static int input(na_core *c, int64_t now, const char *sid, const char *from)
{
    return input_mode(c, now, sid, from, NA_MODE_ASK);
}

static int turn(na_core *c, int64_t now, const char *sid, na_turn_reason reason,
                const na_call *calls, size_t n)
{
    na_event ev = { .type = NA_EV_TURN, .sid = sid, .reason = reason, .calls = calls, .ncalls = n };
    return na_core_feed(c, now, &ev);
}

static int tool_done(na_core *c, int64_t now, const char *sid, const char *id, bool ok, const char *content)
{
    na_event ev = { .type = NA_EV_TOOL_DONE, .sid = sid, .id = id, .ok = ok, .content = content };
    return na_core_feed(c, now, &ev);
}

static int approve(na_core *c, int64_t now, const char *sid, const char *id, bool ok)
{
    na_event ev = { .type = NA_EV_APPROVE, .sid = sid, .id = id, .ok = ok, .from = "tg:1" };
    return na_core_feed(c, now, &ev);
}

static int simple(na_core *c, int64_t now, na_ev_type type, const char *sid)
{
    na_event ev = { .type = type, .sid = sid };
    return na_core_feed(c, now, &ev);
}

/* ---- 用例 ---- */

static void test_final_answer(void)
{
    na_core *c = mk(16);
    CHECK(input(c, 0, "s1", "tg:9") == 0);
    EXPECT(c, "MODEL(s1)");
    CHECK(turn(c, 10, "s1", NA_TURN_FINAL, NULL, 0) == 0);
    EXPECT(c, "DONE(ok)");
    CHECK(na_core_active_runs(c) == 0);
    na_core_free(c);
}

static void test_tool_roundtrip(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:9");
    EXPECT(c, "MODEL(s1)");
    na_call calls[] = { { "c1", "Read", "{}", true, NA_SB_DEFAULT, NULL } };
    CHECK(turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 1) == 0);
    na_cmd *m = na_core_next(c);
    CHECK(m && m->type == NA_CMD_EXEC && !strcmp(m->name, "Read") && m->timeout_ms == 5000);
    CHECK(m && m->policy && !strcmp(m->policy, "{\"dirs\":[\"/w\"]}"));   /* 执行约束原样带给执行器 */
    CHECK(m && !strcmp(m->from, "tg:9"));
    na_cmd_free(m);
    EXPECT(c, "");
    CHECK(tool_done(c, 20, "s1", "c1", true, "12:00") == 0);
    EXPECT(c, "RESULT(c1,ok,12:00) MODEL(s1)");
    turn(c, 30, "s1", NA_TURN_FINAL, NULL, 0);
    EXPECT(c, "DONE(ok)");
    na_core_free(c);
}

static void test_parallel_calls_wait_for_all(void)
{
    na_core *c = mk(16);
    input_mode(c, 0, "s1", "tg:1", NA_MODE_AUTO);
    drain(c);
    na_call calls[] = { { "a", "Read", "{}", true, NA_SB_DEFAULT, NULL }, { "b", "Write", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 2);
    EXPECT(c, "EXEC(a,Read,5000) EXEC(b,Write,10000)");
    tool_done(c, 20, "s1", "b", true, "B");
    EXPECT(c, "RESULT(b,ok,B)");                 /* a 还没完成，不能请求模型 */
    tool_done(c, 30, "s1", "a", false, "boom");
    EXPECT(c, "RESULT(a,err,boom) MODEL(s1)");
    na_core_free(c);
}

static void test_rejections_are_results(void)
{
    na_core *c = mk(16);
    input_mode(c, 0, "s1", "tg:9", NA_MODE_NONE);   /* 比如群里的非管理员 */
    drain(c);
    na_call calls[] = {
        { "x", "nope", "{}", true, NA_SB_DEFAULT, NULL },
        { "y", "Read", "not json", false, NA_SB_DEFAULT, NULL },
        { "z", "Read", "{}", true, NA_SB_DEFAULT, NULL },
    };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 3);
    EXPECT(c, "RESULT(x,err,未知工具：nope) "
              "RESULT(y,err,参数不是合法的 JSON 对象：not json) "
              "RESULT(z,err,当前身份没有使用工具的权限) "
              "MODEL(s1)");
    na_core_free(c);
}

/* 四种模式 × 三类工具 */
static void test_mode_matrix(void)
{
    na_call calls[] = { { "r", "Read", "{}", true, NA_SB_DEFAULT, NULL }, { "w", "Write", "{}", true, NA_SB_DEFAULT, NULL }, { "s", "Shell", "{}", true, NA_SB_DEFAULT, NULL } };
    struct { na_mode mode; bool sandbox; const char *want; } cases[] = {
        { NA_MODE_READONLY, true,
          "EXEC(r,Read,5000) RESULT(w,err,当前是只读模式，只能用 Read 和 Grep) "
          "RESULT(s,err,当前是只读模式，只能用 Read 和 Grep)" },
        { NA_MODE_ASK, true, "EXEC(r,Read,5000) APPROVE_REQ(w,Write,tg:1) APPROVE_REQ(s,Shell,tg:1)" },
        { NA_MODE_AUTO, true, "EXEC(r,Read,5000) EXEC(w,Write,10000) EXEC(s,Shell,30000)" },
        /* 没有沙箱时，自动模式下的 Shell 退回到审批 */
        { NA_MODE_AUTO, false, "EXEC(r,Read,5000) EXEC(w,Write,10000) APPROVE_REQ(s,Shell,tg:1)" },
    };
    for (size_t i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        na_core *c = mk_sb(16, cases[i].sandbox);
        input_mode(c, 0, "s1", "tg:1", cases[i].mode);
        drain(c);
        turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 3);
        EXPECT(c, cases[i].want);
        na_core_free(c);
    }
}

/* 每一轮可以指定执行工具的机器有没有沙箱（远程执行时和本机不同） */
static void test_per_run_sandbox(void)
{
    na_call calls[] = { { "s", "Shell", "{}", true, NA_SB_DEFAULT, NULL } };
    struct { bool cfg; na_sandbox run; const char *want; } cases[] = {
        { true, NA_SB_NO, "APPROVE_REQ(s,Shell,tg:1)" },   /* 本机有沙箱，这一轮的服务器没有 */
        { false, NA_SB_YES, "EXEC(s,Shell,30000)" },        /* 反过来 */
        { true, NA_SB_DEFAULT, "EXEC(s,Shell,30000)" },
    };
    for (size_t i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        na_core *c = mk_sb(16, cases[i].cfg);
        na_event ev = { .type = NA_EV_INPUT, .sid = "s1", .from = "tg:1", .mode = NA_MODE_AUTO,
                        .sandbox = cases[i].run, .policy = "{}" };
        CHECK(na_core_feed(c, 0, &ev) == 0);
        drain(c);
        turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 1);
        EXPECT(c, cases[i].want);
        na_core_free(c);
    }
}

/* 同一轮里不同调用在不同机器上执行：沙箱按调用算；宿主预检出的错误直接作为结果 */
static void test_per_call_sandbox_and_error(void)
{
    na_core *c = mk_sb(16, true);
    na_event ev = { .type = NA_EV_INPUT, .sid = "s1", .from = "tg:1", .mode = NA_MODE_AUTO, .policy = "{}" };
    CHECK(na_core_feed(c, 0, &ev) == 0);
    drain(c);
    na_call calls[] = {
        { "a", "Shell", "{}", true, NA_SB_NO, NULL },           /* 那台机器没有沙箱：要审批 */
        { "b", "Shell", "{}", true, NA_SB_YES, NULL },          /* 有沙箱：直接执行 */
        { "c", "Read", "{}", true, NA_SB_DEFAULT, "没有叫 x 的工作目录" },
    };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 3);
    EXPECT(c, "APPROVE_REQ(a,Shell,tg:1) EXEC(b,Shell,30000) RESULT(c,err,没有叫 x 的工作目录)");
    CHECK(na_core_policy(c, "s1") && !strcmp(na_core_policy(c, "s1"), "{}"));
    CHECK(na_core_policy(c, "nope") == NULL);
    na_core_free(c);
}

static void test_approval_flow(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:1");
    drain(c);
    na_call calls[] = { { "c1", "Shell", "{\"command\":\"ls\"}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 1);
    EXPECT(c, "APPROVE_REQ(c1,Shell,tg:1)");

    CHECK(approve(c, 30, "s1", "c1", true) == 0);
    EXPECT(c, "NOTICE(已批准，开始执行 Shell) EXEC(c1,Shell,30000)");
    CHECK(approve(c, 31, "s1", "c1", true) == -1);  /* 重复审批被忽略 */
    drain(c);

    tool_done(c, 40, "s1", "c1", true, "ok");
    EXPECT(c, "RESULT(c1,ok,ok) MODEL(s1)");
    na_core_free(c);
}

static void test_approval_denied_and_timeout(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:1");
    drain(c);
    na_call one[] = { { "c1", "Shell", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, one, 1);
    drain(c);
    approve(c, 20, "s1", "c1", false);
    EXPECT(c, "RESULT(c1,err,管理员拒绝执行) MODEL(s1)");

    na_call two[] = { { "c2", "Shell", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 30, "s1", NA_TURN_TOOL_USE, two, 1);
    drain(c);
    CHECK(na_core_next_deadline(c) == 30 + 120000);
    simple(c, 30 + 119999, NA_EV_TICK, NULL);
    EXPECT(c, "");
    simple(c, 30 + 120000, NA_EV_TICK, NULL);
    EXPECT(c, "RESULT(c2,err,审批超时，未执行) MODEL(s1)");
    na_core_free(c);
}

static void test_tool_timeout_and_late_result(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:9");
    drain(c);
    na_call calls[] = { { "c1", "Read", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 1);
    drain(c);
    CHECK(na_core_next_deadline(c) == 10 + 5000);
    simple(c, 10 + 5000, NA_EV_TICK, NULL);
    EXPECT(c, "KILL(c1) RESULT(c1,err,执行超时（5 秒），已终止) MODEL(s1)");
    CHECK(tool_done(c, 6000, "s1", "c1", true, "late") == -1);
    EXPECT(c, "");
    na_core_free(c);
}

static void test_step_budget(void)
{
    na_core *c = mk(2);
    input(c, 0, "s1", "tg:9");
    EXPECT(c, "MODEL(s1)");
    na_call a[] = { { "a", "Read", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 1, "s1", NA_TURN_TOOL_USE, a, 1);
    drain(c);
    tool_done(c, 2, "s1", "a", true, "1");
    EXPECT(c, "RESULT(a,ok,1) MODEL(s1)");
    na_call b[] = { { "b", "Read", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 3, "s1", NA_TURN_TOOL_USE, b, 1);
    drain(c);
    tool_done(c, 4, "s1", "b", true, "2");
    EXPECT(c, "RESULT(b,ok,2) DONE(budget)");
    CHECK(na_core_active_runs(c) == 0);
    na_core_free(c);
}

/* 网关每一轮带上自己的上限：大于 0 是上限，小于 0 不限，0 用内核配置 */
static void test_gateway_limits(void)
{
    na_core *c = mk(16);
    na_event ev = { .type = NA_EV_INPUT, .sid = "s1", .from = "tg:9", .mode = NA_MODE_ASK, .max_steps = 1 };
    na_core_feed(c, 0, &ev);
    EXPECT(c, "MODEL(s1)");
    na_call a[] = { { "a", "Read", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 1, "s1", NA_TURN_TOOL_USE, a, 1);
    drain(c);
    tool_done(c, 2, "s1", "a", true, "1");
    EXPECT(c, "RESULT(a,ok,1) DONE(budget)");
    na_core_free(c);

    /* 不限次数、不限时长：请求 40 次模型（内核配置是 2）也不停，没有截止时刻 */
    c = mk(2);
    ev = (na_event){ .type = NA_EV_INPUT, .sid = "s1", .from = "tg:9", .mode = NA_MODE_ASK,
                     .max_steps = -1, .run_timeout_ms = -1, .approval_timeout_ms = -1 };
    na_core_feed(c, 0, &ev);
    EXPECT(c, "MODEL(s1)");
    CHECK(na_core_next_deadline(c) == -1);
    for (int i = 0; i < 40; i++) {
        turn(c, 10 + i, "s1", NA_TURN_TOOL_USE, a, 1);
        drain(c);
        tool_done(c, 10 + i, "s1", "a", true, "1");
        EXPECT(c, "RESULT(a,ok,1) MODEL(s1)");
    }
    simple(c, 100000000, NA_EV_TICK, NULL);
    EXPECT(c, "");
    /* 一直等审批：截止时刻只有工具自己的 */
    na_call w[] = { { "w", "Write", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 100000001, "s1", NA_TURN_TOOL_USE, w, 1);
    EXPECT(c, "APPROVE_REQ(w,Write,tg:9)");
    CHECK(na_core_next_deadline(c) == -1);
    simple(c, 900000000, NA_EV_TICK, NULL);
    EXPECT(c, "");
    approve(c, 900000001, "s1", "w", true);
    EXPECT(c, "NOTICE(已批准，开始执行 Write) EXEC(w,Write,10000)");
    CHECK(na_core_next_deadline(c) == 900010001);
    tool_done(c, 900000002, "s1", "w", true, "ok");
    EXPECT(c, "RESULT(w,ok,ok) MODEL(s1)");
    turn(c, 900000003, "s1", NA_TURN_FINAL, NULL, 0);
    EXPECT(c, "DONE(ok)");
    na_core_free(c);

    /* 0：用内核配置 */
    c = mk(1);
    ev = (na_event){ .type = NA_EV_INPUT, .sid = "s1", .from = "tg:9", .mode = NA_MODE_ASK };
    na_core_feed(c, 0, &ev);
    drain(c);
    CHECK(na_core_next_deadline(c) == 600000);
    turn(c, 1, "s1", NA_TURN_TOOL_USE, a, 1);
    drain(c);
    tool_done(c, 2, "s1", "a", true, "1");
    EXPECT(c, "RESULT(a,ok,1) DONE(budget)");
    na_core_free(c);
}

static void test_cancel(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:1");
    drain(c);
    na_call calls[] = { { "a", "Read", "{}", true, NA_SB_DEFAULT, NULL }, { "b", "Shell", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 2);
    EXPECT(c, "EXEC(a,Read,5000) APPROVE_REQ(b,Shell,tg:1)");
    CHECK(simple(c, 20, NA_EV_CANCEL, "s1") == 0);
    EXPECT(c, "KILL(a) RESULT(a,err,已取消) RESULT(b,err,已取消) DONE(cancelled)");
    CHECK(simple(c, 21, NA_EV_CANCEL, "s1") == -1);
    na_core_free(c);
}

static void test_run_timeout_while_waiting_model(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:9");
    drain(c);
    CHECK(na_core_next_deadline(c) == 600000);
    simple(c, 600000, NA_EV_TICK, NULL);
    EXPECT(c, "DONE(timeout)");
    CHECK(turn(c, 600001, "s1", NA_TURN_FINAL, NULL, 0) == -1);  /* 迟到的模型结果 */
    na_core_free(c);
}

static void test_reset_kills_running_tools(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:9");
    input(c, 0, "s2", "tg:9");
    drain(c);
    na_call calls[] = { { "a", "Read", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 1);
    drain(c);
    simple(c, 20, NA_EV_RESET, NULL);
    EXPECT(c, "KILL(a)");
    CHECK(na_core_active_runs(c) == 0);
    CHECK(na_core_next_deadline(c) == -1);
    na_core_free(c);
}

static void test_protocol_mismatches_are_ignored(void)
{
    na_core *c = mk(16);
    CHECK(turn(c, 0, "nobody", NA_TURN_FINAL, NULL, 0) == -1);
    CHECK(tool_done(c, 0, "nobody", "x", true, "") == -1);
    input(c, 0, "s1", "tg:9");
    CHECK(input(c, 1, "s1", "tg:9") == -1);       /* 同一会话同时只能有一个 run */
    drain(c);
    na_call calls[] = { { "a", "Read", "{}", true, NA_SB_DEFAULT, NULL } };
    turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 1);
    drain(c);
    CHECK(turn(c, 11, "s1", NA_TURN_FINAL, NULL, 0) == -1);   /* 还在等工具，不该收到 turn */
    CHECK(tool_done(c, 12, "s1", "other", true, "") == -1);
    EXPECT(c, "");
    na_core_free(c);
}

static void test_turn_error_and_refusal(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:9");
    drain(c);
    turn(c, 1, "s1", NA_TURN_ERROR, NULL, 0);
    EXPECT(c, "DONE(error)");
    input(c, 2, "s1", "tg:9");
    drain(c);
    turn(c, 3, "s1", NA_TURN_REFUSED, NULL, 0);
    EXPECT(c, "DONE(refused)");
    input(c, 4, "s1", "tg:9");
    drain(c);
    turn(c, 5, "s1", NA_TURN_TOOL_USE, NULL, 0);   /* 声称要调工具但没有调用 */
    EXPECT(c, "DONE(ok)");
    na_core_free(c);
}

static void test_long_args_preview_keeps_utf8(void)
{
    na_core *c = mk(16);
    input(c, 0, "s1", "tg:9");
    drain(c);
    char args[400];
    size_t n = 0;
    while (n + 3 < sizeof args - 1) { memcpy(args + n, "中", 3); n += 3; }
    args[n] = 0;
    na_call calls[] = { { "a", "Read", args, false, NA_SB_DEFAULT, NULL } };
    turn(c, 1, "s1", NA_TURN_TOOL_USE, calls, 1);
    na_cmd *m = na_core_next(c);
    CHECK(m && m->type == NA_CMD_RESULT);
    if (m) {
        const char *p = strstr(m->text, "：");
        CHECK(p != NULL);
        /* 198 = 200 往前退到字符边界；之后是 "..." */
        CHECK(p && strlen(p + strlen("：")) == 198 + 3);
        na_cmd_free(m);
    }
    drain(c);
    na_core_free(c);
}


static void test_elevated_always_needs_approval(void)
{
    na_core *c = mk(16);
    na_event ev = { .type = NA_EV_INPUT, .sid = "s1", .from = "tg:1", .mode = NA_MODE_AUTO,
                    .elevate = true, .policy = "{\"dirs\":[\"/w\"]}" };
    CHECK(na_core_feed(c, 0, &ev) == 0);
    drain(c);
    na_call calls[] = { { "w1", "Write", "{}", true, NA_SB_DEFAULT, NULL, true } };
    CHECK(turn(c, 10, "s1", NA_TURN_TOOL_USE, calls, 1) == 0);
    EXPECT(c, "APPROVE_REQ(w1,Write,tg:1)");
    CHECK(approve(c, 20, "s1", "w1", true) == 0);
    EXPECT(c, "NOTICE(已批准，开始执行 Write) EXEC(w1,Write,10000)");
    tool_done(c, 30, "s1", "w1", true, "ok");
    drain(c);
    na_core_free(c);

    /* 访客不能提权 */
    c = mk(16);
    ev = (na_event){ .type = NA_EV_INPUT, .sid = "s2", .from = "tg:2", .mode = NA_MODE_AUTO,
                     .elevate = false, .policy = "{\"dirs\":[\"/w\"]}" };
    CHECK(na_core_feed(c, 0, &ev) == 0);
    drain(c);
    na_call guest[] = { { "w2", "Write", "{}", true, NA_SB_DEFAULT, NULL, true } };
    CHECK(turn(c, 10, "s2", NA_TURN_TOOL_USE, guest, 1) == 0);
    EXPECT(c, "RESULT(w2,err,当前身份不能提权写工作目录外) MODEL(s2)");
    na_core_free(c);

    /* 只读模式不能提权 */
    c = mk(16);
    ev = (na_event){ .type = NA_EV_INPUT, .sid = "s3", .from = "tg:3", .mode = NA_MODE_READONLY,
                     .elevate = true, .policy = "{\"dirs\":[\"/w\"]}" };
    CHECK(na_core_feed(c, 0, &ev) == 0);
    drain(c);
    na_call ro[] = { { "w3", "Write", "{}", true, NA_SB_DEFAULT, NULL, true } };
    CHECK(turn(c, 10, "s3", NA_TURN_TOOL_USE, ro, 1) == 0);
    EXPECT(c, "RESULT(w3,err,当前是只读模式，不能提权写工作目录外) MODEL(s3)");
    na_core_free(c);
}


static void test_admin_tool_always_approves(void)
{
    na_core *c = mk(16);
    na_event ev = { .type = NA_EV_INPUT, .sid = "a1", .from = "tg:1", .mode = NA_MODE_AUTO,
                    .elevate = true, .policy = "{}" };
    CHECK(na_core_feed(c, 0, &ev) == 0);
    drain(c);
    na_call calls[] = { { "a", "GatewayAdmin", "{}", true, NA_SB_DEFAULT, NULL } };
    CHECK(turn(c, 10, "a1", NA_TURN_TOOL_USE, calls, 1) == 0);
    EXPECT(c, "APPROVE_REQ(a,GatewayAdmin,tg:1)");
    na_core_free(c);

    c = mk(16);
    ev = (na_event){ .type = NA_EV_INPUT, .sid = "a2", .from = "tg:1", .mode = NA_MODE_READONLY, .elevate = true, .policy = "{}" };
    CHECK(na_core_feed(c, 0, &ev) == 0);
    drain(c);
    na_call ro[] = { { "b", "GatewayAdmin", "{}", true, NA_SB_DEFAULT, NULL } };
    CHECK(turn(c, 10, "a2", NA_TURN_TOOL_USE, ro, 1) == 0);
    EXPECT(c, "RESULT(b,err,当前是只读模式，不能使用网关管理工具) MODEL(a2)");
    na_core_free(c);
}

static void test_public_read_all_modes(void)
{
    na_mode modes[] = {NA_MODE_NONE, NA_MODE_READONLY, NA_MODE_ASK, NA_MODE_AUTO};
    for (size_t i = 0; i < sizeof modes / sizeof modes[0]; i++) {
        na_core *c = mk(16);
        input_mode(c, 0, "s", "guest", modes[i]);
        drain(c);
        na_call calls[] = {{"w", "WebSearch", "{}", true, NA_SB_DEFAULT, NULL}};
        turn(c, 1, "s", NA_TURN_TOOL_USE, calls, 1);
        EXPECT(c, "EXEC(w,WebSearch,70000)");
        na_core_free(c);
    }
}

static void test_schedule_policy_and_deadlines(void)
{
    na_mode modes[]={NA_MODE_READONLY,NA_MODE_ASK,NA_MODE_AUTO};
    const char *wants[]={
        "EXEC(l,Schedule,70000) RESULT(w,err,当前是只读模式，只能用 Read 和 Grep) EXEC(r,Schedule,70000)",
        "EXEC(l,Schedule,70000) APPROVE_REQ(w,Schedule,tg:1) EXEC(r,Schedule,70000)",
        "EXEC(l,Schedule,70000) EXEC(w,Schedule,70000) EXEC(r,Schedule,70000)"
    };
    for (size_t i=0;i<3;i++) {
        na_core *c=mk(16);
        na_event ev={.type=NA_EV_INPUT,.sid="s",.from="tg:1",.mode=modes[i],.elevate=true};
        na_core_feed(c,0,&ev);drain(c);
        na_call calls[]={
            {.id="l",.name="Schedule",.args="{}",.args_ok=true,.action="list"},
            {.id="w",.name="Schedule",.args="{}",.args_ok=true,.action="create"},
            {.id="r",.name="Schedule",.args="{}",.args_ok=true,.action="run_now"}
        };
        turn(c,1,"s",NA_TURN_TOOL_USE,calls,3);
        EXPECT(c,wants[i]);na_core_free(c);
    }
    na_core *c=mk(16);
    na_event ev={.type=NA_EV_INPUT,.sid="s",.from="tg:1",.mode=NA_MODE_ASK,.elevate=true,
                 .run_timeout_ms=4000000,.approval_timeout_ms=1800000};
    na_core_feed(c,0,&ev);drain(c);
    CHECK(na_core_next_deadline(c)==4000000);
    na_call call={.id="w",.name="Write",.args="{}",.args_ok=true};
    turn(c,10,"s",NA_TURN_TOOL_USE,&call,1);drain(c);
    CHECK(na_core_next_deadline(c)==1800010);
    simple(c,1800010,NA_EV_TICK,NULL);
    EXPECT(c,"RESULT(w,err,审批超时，未执行) MODEL(s)");
    na_core_free(c);
    c=mk(16);
    ev=(na_event){.type=NA_EV_INPUT,.sid="s",.from="tg:1",.mode=NA_MODE_READONLY,.elevate=true};
    na_core_feed(c,0,&ev);drain(c);
    call=(na_call){.id="d",.name="GatewayAdmin",.args="{}",.args_ok=true,.action="history_delete"};
    turn(c,1,"s",NA_TURN_TOOL_USE,&call,1);
    EXPECT(c,"APPROVE_REQ(d,GatewayAdmin,tg:1)");
    na_core_free(c);
}

int main(void)
{
    test_schedule_policy_and_deadlines();
    test_public_read_all_modes();
    test_final_answer();
    test_tool_roundtrip();
    test_parallel_calls_wait_for_all();
    test_rejections_are_results();
    test_mode_matrix();
    test_per_run_sandbox();
    test_per_call_sandbox_and_error();
    test_approval_flow();
    test_approval_denied_and_timeout();
    test_tool_timeout_and_late_result();
    test_step_budget();
    test_gateway_limits();
    test_cancel();
    test_run_timeout_while_waiting_model();
    test_reset_kills_running_tools();
    test_protocol_mismatches_are_ignored();
    test_turn_error_and_refusal();
    test_long_args_preview_keeps_utf8();
    test_elevated_always_needs_approval();
    test_admin_tool_always_approves();
    printf("core: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
}
