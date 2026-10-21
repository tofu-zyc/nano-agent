/* main.c —— nano-kernel：连接网关，驱动状态机，执行本地工具
 *
 * 单线程 poll 循环：网关连接、工具子进程的管道、信号唤醒管道都在同一个 poll 里。
 * 外部消息 → 事件 → na_core_feed；na_core_next 取出的命令 → 发给网关或交给执行器。
 */
#include "cJSON.h"
#include "conf.h"
#include "core.h"
#include "link.h"
#include "tools.h"
#include "util.h"

#include <errno.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define PROTO_VERSION      1
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

typedef struct {
    kconf           conf;
    toolset_t       tools;
    na_tool_policy *policies;
    na_core        *core;
    runner_t        runner;
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
    cJSON_AddStringToObject(m, "node", k->conf.node);
    cJSON *arr = cJSON_AddArrayToObject(m, "tools");
    for (size_t i = 0; i < k->tools.n; i++)
        cJSON_AddItemToArray(arr, cJSON_Duplicate(k->tools.v[i].desc, 1));
    link_send(&k->link, m);
}

/* ---- 命令 → 外部动作 ---- */

static void exec_tool(kernel_t *k, const na_cmd *c, int64_t now)
{
    const tool_t *t = toolset_find(&k->tools, c->name);
    char err[256] = "";
    if (t && runner_exec(&k->runner, t, c, err, sizeof err) == 0) {
        log_info("执行 %s（%s）", c->name, c->id);
        return;
    }
    if (!t) snprintf(err, sizeof err, "工具 %s 不存在", c->name);
    log_warn("无法执行 %s：%s", c->name, err);
    na_event ev = { .type = NA_EV_TOOL_DONE, .sid = c->sid, .id = c->id, .ok = false, .content = err };
    na_core_feed(k->core, now, &ev);
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
            if (!c->ok) log_info("调用 %s 没有成功：%.200s", c->id, c->text ? c->text : "");
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
        case NA_CMD_KILL:
            runner_kill(&k->runner, c->sid, c->id);
            log_info("终止 %s", c->id);
            break;
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

static void handle_msg(kernel_t *k, const cJSON *m, int64_t now)
{
    const char *t = sget(m, "t");
    if (!t) return;
    if (!strcmp(t, "pong")) return;
    if (!strcmp(t, "welcome")) {
        log_info("网关已接受连接，报告了 %zu 个工具", k->tools.n);
        return;
    }
    if (!strcmp(t, "error")) {
        log_error("网关报错：%s", sget(m, "detail") ? sget(m, "detail") : "（无详情）");
        return;
    }

    na_event ev = { .sid = sget(m, "sid") };
    na_call *calls = NULL;
    char **owned = NULL;
    size_t ncalls = 0;

    if (!strcmp(t, "input")) {
        ev.type = NA_EV_INPUT;
        ev.from = sget(m, "from");
        log_info("run 开始 sid=%s from=%s", ev.sid ? ev.sid : "-", ev.from ? ev.from : "-");
    } else if (!strcmp(t, "turn")) {
        ev.type = NA_EV_TURN;
        ev.reason = parse_reason(sget(m, "reason"));
        ev.detail = sget(m, "detail");
        const cJSON *arr = cJSON_GetObjectItemCaseSensitive(m, "calls");
        ncalls = cJSON_IsArray(arr) ? (size_t)cJSON_GetArraySize(arr) : 0;
        if (ncalls) {
            calls = xcalloc(ncalls, sizeof *calls);
            owned = xcalloc(ncalls, sizeof *owned);
        }
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

    for (size_t i = 0; i < ncalls; i++) cJSON_free(owned[i]);
    free(owned);
    free(calls);
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
    k->backoff = 1000;
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
    k->next_connect = now + k->backoff;
}

static void collect_finished(kernel_t *k, int64_t now)
{
    job_t *j;
    while ((j = runner_take_finished(&k->runner, now))) {
        if (!j->killed) {
            bool ok;
            char *res = job_result(j, k->conf.max_output, &ok);
            log_info("%s（%s）%s", j->name, j->id, ok ? "完成" : "失败");
            na_event ev = { .type = NA_EV_TOOL_DONE, .sid = j->sid, .id = j->id, .ok = ok, .content = res };
            na_core_feed(k->core, now, &ev);
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
                        log_debug("<- %.500s", s);
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
            "  -t 只检查配置和工具，然后退出\n"
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
    if (kconf_load(&k.conf, conf_path, err, sizeof err) < 0) {
        log_error("%s", err);
        return 1;
    }
    if (toolset_load(&k.tools, k.conf.tools_dir) < 0) {
        log_error("打不开工具目录 %s：%s", k.conf.tools_dir, strerror(errno));
        return 1;
    }

    buf_t names = {0};
    for (size_t i = 0; i < k.tools.n; i++) {
        const na_tool_policy *p = &k.tools.v[i].pol;
        char item[160];
        snprintf(item, sizeof item, "%s%s[%s%s, %ds]", i ? " " : "", p->name,
                 p->allow_all ? "所有人" : "管理员", p->approval ? ", 需审批" : "", p->timeout_ms / 1000);
        buf_puts(&names, item);
    }
    log_info("节点 %s，网关 %s", k.conf.node, k.conf.gateway);
    log_info("%zu 个工具：%s", k.tools.n, k.tools.n ? buf_cstr(&names) : "（无）");
    log_info("%zu 个管理员", k.conf.nadmins);
    buf_free(&names);
    if (check_only) {
        toolset_free(&k.tools);
        kconf_free(&k.conf);
        return 0;
    }

    k.policies = xcalloc(k.tools.n, sizeof *k.policies);
    for (size_t i = 0; i < k.tools.n; i++) k.policies[i] = k.tools.v[i].pol;
    na_core_config cc = {
        .tools = k.policies, .ntools = k.tools.n,
        .admins = (const char *const *)k.conf.admins, .nadmins = k.conf.nadmins,
        .max_steps = k.conf.max_steps,
        .run_timeout_ms = k.conf.run_timeout * 1000,
        .approval_timeout_ms = k.conf.approval_timeout * 1000,
    };
    k.core = na_core_new(&cc);
    k.runner.max_output = k.conf.max_output;

    setup_signals();
    loop(&k);

    log_info("退出");
    runner_kill_all(&k.runner);
    runner_reap(&k.runner, now_ms());
    for (job_t *j; (j = k.runner.jobs);) {
        k.runner.jobs = j->next;
        job_free(j);
    }
    link_close(&k.link);
    buf_free(&k.link.in);
    buf_free(&k.link.out);
    na_core_free(k.core);
    free(k.policies);
    toolset_free(&k.tools);
    kconf_free(&k.conf);
    return 0;
}
