/* main.c —— nano-kernel：连接网关，驱动状态机，调执行器执行工具
 *
 * 单线程 poll 循环：网关连接、执行器的管道、信号唤醒管道都在同一个 poll 里。
 * 外部消息 → 事件 → na_core_feed；na_core_next 取出的命令 → 发给网关或交给执行器。
 */
#include "cJSON.h"
#include "conf.h"
#include "core.h"
#include "link.h"
#include "remote.h"
#include "runner.h"
#include "skills.h"
#include "tools.h"
#include "util.h"

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define PROTO_VERSION      3
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
    const na_tool_policy *tools;
    size_t          ntools;
    int             landlock;    /* 执行器探测到的 Landlock 版本，0 表示没有沙箱 */
    bool            rg;
    char           *exec_real;   /* 执行器的真实路径，它不能落在工作目录里 */
    na_core        *core;
    runner_t        runner;
    remotes_t       remotes;     /* 通过 SSH 执行工具的服务器 */
    skills_t        skills;      /* 技能 */
    int64_t         skills_at;   /* 下一次重新扫描技能目录的时刻 */
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

/* 给模型看的工具：内置工具，有技能时加上 Skill（按名字排序） */
static cJSON *all_tools(kernel_t *k)
{
    cJSON *arr = tools_specs();
    cJSON *sk = skills_tool_spec(&k->skills);
    if (!sk) return arr;
    int i = 0;
    const cJSON *it;
    cJSON_ArrayForEach(it, arr) {
        if (strcmp(cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(it, "name")), "Skill") > 0) break;
        i++;
    }
    cJSON_InsertItemInArray(arr, i, sk);
    return arr;
}

static void send_hello(kernel_t *k)
{
    cJSON *m = msg_new("hello", NULL);
    cJSON_AddNumberToObject(m, "v", PROTO_VERSION);
    cJSON_AddStringToObject(m, "token", k->conf.token);
    if (!*k->conf.token && strcmp(k->conf.gateway, "stdio")) cJSON_AddBoolToObject(m, "pair", true);
    cJSON_AddStringToObject(m, "node", k->conf.node);
    cJSON_AddItemToObject(m, "tools", all_tools(k));
    cJSON_AddItemToObject(m, "skills", skills_json(&k->skills));
    cJSON *sb = cJSON_AddObjectToObject(m, "sandbox");
    cJSON_AddNumberToObject(sb, "landlock", k->landlock);
    cJSON_AddItemToObject(m, "remotes", remotes_json(&k->remotes));
    link_send(&k->link, m);
}

/* 技能变了：重新报一遍工具 */
static void send_tools(kernel_t *k)
{
    if (k->link.fd < 0) return;
    cJSON *m = msg_new("tools", NULL);
    cJSON_AddItemToObject(m, "tools", all_tools(k));
    cJSON_AddItemToObject(m, "skills", skills_json(&k->skills));
    link_send(&k->link, m);
}

static void refresh_skills(kernel_t *k, int64_t now)
{
    k->skills_at = now + 30000;
    if (skills_scan(&k->skills, k->conf.skills)) {
        log_info("技能有变化：%zu 个", k->skills.n);
        send_tools(k);
    }
}

/* 服务器能不能用变了，告诉网关 */
static void send_remotes(kernel_t *k)
{
    if (k->link.fd < 0) return;
    cJSON *m = msg_new("remotes", NULL);
    cJSON_AddItemToObject(m, "remotes", remotes_json(&k->remotes));
    link_send(&k->link, m);
}

/* input 里的 node：本机（空或者等于自己的名字）返回 NULL；服务器返回它；不认识时 *unknown 为 true */
static remote_t *target_of(kernel_t *k, const char *node, bool *unknown)
{
    *unknown = false;
    if (!node || !*node || !strcmp(node, k->conf.node)) return NULL;
    remote_t *rm = remotes_find(&k->remotes, node);
    if (!rm) *unknown = true;
    return rm;
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

/* 服务器上的目录：在服务器上跑 nano-exec --ls，结束后在 collect_finished 里回复 */
static void remote_ls(kernel_t *k, remote_t *rm, const char *id, const char *p, int64_t now)
{
    char err[256] = "";
    if (rm->state != RM_READY) {
        snprintf(err, sizeof err, "服务器 %s 现在用不了：%s", rm->conf->name, rm->detail ? rm->detail : "还在准备");
    } else {
        char *qp = shell_quote(p && *p ? p : "~");
        char *args = xasprintf("--ls %s", qp);
        char **argv = remote_exec_argv(&k->remotes, rm, args);
        job_t *j = runner_start_ex(&k->runner, argv, "", id ? id : "", rm->conf->name, xstrdup(""), 0, err, sizeof err);
        argv_free(argv);
        free(args);
        free(qp);
        if (j) {
            j->kind = JOB_LS;
            j->tag = remotes_index(&k->remotes, rm);
            j->close_in = true;
            return;
        }
    }
    (void)now;
    cJSON *r = msg_new("ls_result", NULL);
    cJSON_AddStringToObject(r, "id", id ? id : "");
    cJSON_AddBoolToObject(r, "ok", false);
    cJSON_AddStringToObject(r, "detail", err);
    link_send(&k->link, r);
}

static void handle_ls(kernel_t *k, const cJSON *m, int64_t now)
{
    const char *id = sget(m, "id"), *p = sget(m, "path");
    bool unknown;
    remote_t *rm = target_of(k, sget(m, "node"), &unknown);
    if (rm || unknown) {
        if (rm) {
            remote_ls(k, rm, id, p, now);
            return;
        }
        cJSON *r = msg_new("ls_result", NULL);
        cJSON_AddStringToObject(r, "id", id ? id : "");
        cJSON_AddBoolToObject(r, "ok", false);
        cJSON_AddStringToObject(r, "detail", "这个内核没有配置这台服务器");
        link_send(&k->link, r);
        return;
    }
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

static void exec_tool(kernel_t *k, const na_cmd *c, int64_t now)
{
    char err[512] = "";
    cJSON *req = cJSON_Parse(c->policy ? c->policy : "{}");
    if (!cJSON_IsObject(req)) {
        cJSON_Delete(req);
        req = cJSON_CreateObject();
    }
    bool ok = true, unknown;
    char *node = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(req, "node"));
    remote_t *rm = target_of(k, node, &unknown);
    if (unknown) {
        snprintf(err, sizeof err, "这个内核没有配置服务器 %s", node);
        ok = false;
    } else if (rm && rm->state != RM_READY) {
        snprintf(err, sizeof err, "服务器 %s 现在用不了：%s", rm->conf->name, rm->detail ? rm->detail : "还在准备执行器");
        ok = false;
    }
    if (ok && !strcmp(c->name, "Skill")) {
        /* 说明由内核读；远程执行时先把这个技能的附带文件传到服务器。 */
        cJSON *args = cJSON_Parse(c->args ? c->args : "{}");
        const char *name = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(args, "name"));
        char *root = rm && k->skills.n ? skills_remote_root(&k->skills, k->conf.node) : NULL;
        char *dir = root ? xasprintf("%s/%s", root, name ? name : "") : NULL;
        bool sok;
        char *res = skill_load(&k->skills, name, dir, &sok);
        if (rm && sok) {
            cJSON *reply = cJSON_CreateObject();
            cJSON_AddBoolToObject(reply, "ok", true);
            cJSON_AddStringToObject(reply, "content", res);
            char *body = cJSON_PrintUnformatted(reply);
            char **argv = remote_skill_argv(&k->remotes, rm, skills_find(&k->skills, name)->dir, dir, body);
            job_t *j = runner_start_ex(&k->runner, argv, c->sid, c->id, c->name, xstrdup(""), 0, err, sizeof err);
            argv_free(argv);
            cJSON_free(body);
            cJSON_Delete(reply);
            if (j) {
                j->kind = JOB_REMOTE_TOOL;
                j->tag = remotes_index(&k->remotes, rm);
                j->close_in = true;
            } else {
                sok = false;
                free(res);
                res = xstrdup(err);
            }
            if (j) {
                free(res);
                res = NULL;  /* 上传完成后由 collect_finished 返回结果 */
            }
        }
        log_info("技能 %s（%s）%s", name ? name : "?", c->id, sok ? "" : "没有找到");
        if (res) {
            na_event ev = { .type = NA_EV_TOOL_DONE, .sid = c->sid, .id = c->id, .ok = sok, .content = res };
            na_core_feed(k->core, now, &ev);
        }
        free(res);
        free(dir);
        free(root);
        cJSON_Delete(args);
        cJSON_Delete(req);
        return;
    }
    cJSON_DeleteItemFromObjectCaseSensitive(req, "node");
    /* 工作目录是服务器上的路径时，由服务器上的执行器自己把关 */
    for (const char *const *key = (const char *const[]){ "dirs", "ro", "rw", NULL }; *key && ok && !rm; key++)
        ok = guard_paths(k, cJSON_GetObjectItemCaseSensitive(req, *key), err, sizeof err);
    if (ok && k->skills.n) {
        /* 技能目录在沙箱里只读可见：附带的脚本和资料用 Read 读、用 Shell 运行 */
        cJSON *ro = cJSON_GetObjectItemCaseSensitive(req, "ro");
        if (!cJSON_IsArray(ro)) ro = cJSON_AddArrayToObject(req, "ro");
        char *root = rm ? skills_remote_root(&k->skills, k->conf.node) : xstrdup(k->skills.root);
        cJSON_AddItemToArray(ro, cJSON_CreateString(root));
        free(root);
    }
    if (ok) {
        cJSON *args = cJSON_Parse(c->args ? c->args : "{}");
        cJSON_AddStringToObject(req, "tool", c->name);
        cJSON_AddItemToObject(req, "args", args ? args : cJSON_CreateObject());
        cJSON_AddStringToObject(req, "tmp_key", c->sid);
        cJSON_AddNumberToObject(req, "max_output", (double)k->conf.max_output);
        cJSON_AddBoolToObject(req, "watch_stdin", true);
        char *body = cJSON_PrintUnformatted(req);
        char *line = xasprintf("%s\n", body);
        cJSON_free(body);
        if (rm) {
            char **argv = remote_exec_argv(&k->remotes, rm, NULL);
            job_t *j = runner_start_ex(&k->runner, argv, c->sid, c->id, c->name, line, strlen(line), err, sizeof err);
            argv_free(argv);
            if (j) {
                j->kind = JOB_REMOTE_TOOL;
                j->tag = remotes_index(&k->remotes, rm);
            }
            ok = j != NULL;
            if (ok) log_info("在 %s 上执行 %s（%s）", rm->conf->name, c->name, c->id);
        } else {
            char *argv[] = { k->conf.exec, NULL };
            ok = runner_start(&k->runner, argv, c->sid, c->id, c->name, line, err, sizeof err) == 0;
            if (ok) log_info("执行 %s（%s）", c->name, c->id);
        }
    }
    cJSON_Delete(req);
    if (ok) return;
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

static na_mode parse_mode(const char *s)
{
    if (!s) return NA_MODE_NONE;
    if (!strcmp(s, "auto")) return NA_MODE_AUTO;
    if (!strcmp(s, "ask")) return NA_MODE_ASK;
    if (!strcmp(s, "readonly")) return NA_MODE_READONLY;
    return NA_MODE_NONE;
}

/* input 里的工作目录设置 → 交给执行器的约束（内核状态机不解析，原样带着）。
 * node 是工具在哪台服务器上执行（本机时没有），执行时取出来。 */
static char *build_policy(const cJSON *m, na_mode mode, const char *node)
{
    cJSON *p = cJSON_CreateObject();
    if (node) cJSON_AddStringToObject(p, "node", node);
    const char *keys[] = { "dirs", "ro", "rw" };
    for (size_t i = 0; i < 3; i++) {
        const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, keys[i]);
        cJSON_AddItemToObject(p, keys[i], cJSON_IsArray(v) ? cJSON_Duplicate(v, 1) : cJSON_CreateArray());
    }
    cJSON_AddBoolToObject(p, "write", mode == NA_MODE_ASK || mode == NA_MODE_AUTO);
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
        log_info("等待网关确认接入：打开网关网页的「设备」页面，核对确认码 %s 后点「允许」",
                 sget(m, "code") ? sget(m, "code") : "?");
        return;
    }
    if (!strcmp(t, "paired")) {
        save_token(k, sget(m, "token"), now);
        return;
    }
    if (!strcmp(t, "ls")) {
        handle_ls(k, m, now);
        return;
    }

    na_event ev = { .sid = sget(m, "sid") };
    na_call *calls = NULL;
    char **owned = NULL;
    char *policy = NULL;
    size_t ncalls = 0;

    if (!strcmp(t, "input")) {
        refresh_skills(k, now);
        ev.type = NA_EV_INPUT;
        ev.from = sget(m, "from");
        ev.mode = parse_mode(sget(m, "mode"));
        bool unknown;
        const char *node = sget(m, "node");
        remote_t *rm = target_of(k, node, &unknown);
        /* 不认识的服务器：照常开始，执行工具时报错；有没有沙箱按最保守的算 */
        ev.sandbox = rm ? (rm->landlock > 0 ? NA_SB_YES : NA_SB_NO) : unknown ? NA_SB_NO : NA_SB_DEFAULT;
        policy = build_policy(m, ev.mode, rm || unknown ? node : NULL);
        ev.policy = policy;
        log_info("run 开始 sid=%s from=%s mode=%s%s%s", ev.sid ? ev.sid : "-", ev.from ? ev.from : "-",
                 sget(m, "mode") ? sget(m, "mode") : "none", rm || unknown ? " 在 " : "", rm || unknown ? node : "");
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
    cJSON_free(policy);
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
    if (!strcmp(k->conf.gateway, "stdio")) g_stop = 1; /* SSH 通道关闭后退出，不留下重连进程 */
}

/* 服务器上 --ls 的结果 → ls_result */
static void finish_ls(kernel_t *k, const job_t *j)
{
    cJSON *r = cJSON_ParseWithLength(j->out.p ? j->out.p : "", j->out.len);
    if (!cJSON_IsObject(r)) {
        cJSON_Delete(r);
        r = cJSON_CreateObject();
        cJSON_AddBoolToObject(r, "ok", false);
        bool ok;
        char *why = job_result(j, &ok);
        cJSON_AddStringToObject(r, "detail", why);
        free(why);
    }
    cJSON_AddStringToObject(r, "t", "ls_result");
    cJSON_AddStringToObject(r, "id", j->id);
    link_send(&k->link, r);
}

static void collect_finished(kernel_t *k, int64_t now)
{
    job_t *j;
    while ((j = runner_take_finished(&k->runner, now))) {
        if (j->kind == JOB_PREP) {
            remotes_on_job(&k->remotes, &k->runner, j, now);
        } else if (j->kind == JOB_LS) {
            if (!j->killed) finish_ls(k, j);
        } else if (!j->killed) {
            bool ok;
            char *res = job_result(j, &ok);
            /* ssh 自己失败（退出码 255）又没有执行器的结果：连不上服务器，重新准备 */
            remote_t *rm = j->kind == JOB_REMOTE_TOOL && j->tag >= 0 && (size_t)j->tag < k->remotes.n ? &k->remotes.list[j->tag] : NULL;
            if (rm && !ok && WIFEXITED(j->status) && WEXITSTATUS(j->status) == 255 && !strstr(res, "\"ok\"")) {
                char *why = xasprintf("连不上服务器 %s：%s", rm->conf->name, res);
                remote_failed(&k->remotes, rm, why, now);
                free(res);
                res = why;
            }
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
        int li = -1, wi = -1;
        if (k->link.fd >= 0) {
            li = (int)np;
            bool separate = k->link.write_fd != k->link.fd;
            pfds[np++] = (struct pollfd){ .fd = k->link.fd, .events = POLLIN | (k->link.out.len && !separate ? POLLOUT : 0) };
            if (separate && k->link.out.len) {
                wi = (int)np;
                pfds[np++] = (struct pollfd){ .fd = k->link.write_fd, .events = POLLOUT };
            }
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

        if (wi >= 0 && k->link.fd >= 0 && (pfds[wi].revents & (POLLOUT | POLLERR | POLLHUP)) && link_flush(&k->link) < 0)
            disconnect(k, now, "写入失败");

        if (np > base) runner_on_poll(&k->runner, pfds + base, refs + base, np - base);
        collect_finished(k, now);
        remotes_tick(&k->remotes, &k->runner, now);
        if (now >= k->skills_at) refresh_skills(k, now);
        if (k->remotes.changed) {
            k->remotes.changed = false;
            send_remotes(k);
        }

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
    skills_scan(&k.skills, k.conf.skills);
    k.skills_at = now_ms() + 30000;
    if (remotes_init(&k.remotes, &k.conf, err, sizeof err) < 0) {
        log_error("%s", err);
        free(k.exec_real);
        kconf_free(&k.conf);
        return 1;
    }

    log_info("节点 %s，网关 %s", k.conf.node, k.conf.gateway);
    if (k.skills.n) {
        buf_t names = {0};
        for (size_t i = 0; i < k.skills.n; i++) {
            buf_puts(&names, i ? "、" : "");
            buf_puts(&names, k.skills.list[i].name);
        }
        char *s = buf_cstr(&names);
        log_info("技能目录 %s：%s", k.skills.root, s);
        free(s);
    }
    for (size_t i = 0; i < k.conf.nremotes; i++)
        log_info("服务器 %s：ssh %s，执行器 %s", k.conf.remotes[i].name, k.conf.remotes[i].target,
                 k.conf.remotes[i].exec ? k.conf.remotes[i].exec : "自动上传");
    if (k.landlock > 0)
        log_info("执行器 %s，沙箱 Landlock v%d%s", k.conf.exec, k.landlock, k.rg ? "" : "（没有 rg，Grep 用 grep 代替）");
    else
        log_warn("执行器 %s：本机不支持 Landlock，没有沙箱，自动模式下的 Shell 也需要审批", k.conf.exec);
    if (check_only) {
        skills_free(&k.skills);
        remotes_free(&k.remotes);
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
    remotes_free(&k.remotes);
    skills_free(&k.skills);
    free(k.exec_real);
    kconf_free(&k.conf);
    return 0;
}
