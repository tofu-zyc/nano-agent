/* ctl.c —— nano-kernel 的管理命令：status / start / stop / restart / connect / logs / version / update / uninstall
 *
 * 安装后内核目录里有：nano-kernel、kernel.conf、kernel.log，运行中的内核还会写：
 *   kernel.pid          自己的 pid（文件名跟着配置文件：kernel.conf → kernel.*）
 *   kernel.status.json  和网关的连接状态（连接中 / 已连接 / 等确认 / 被拒绝）
 * 安装脚本写 service（systemd / launchd / task:任务名 / startup:名字 / none），告诉这里内核是怎么在后台运行的；
 * startup 是 Windows 上建不了任务计划时的退路：「启动」文件夹里放一个快捷方式，登录时运行 run-kernel.ps1。
 * Windows 上 stop 在没有服务管理器时写一个 kernel.stop 文件，内核看到后正常退出（Windows 上 run-kernel.ps1 也就不再拉起）。 */
#include "ctl.h"
#include "conf.h"
#include "cJSON.h"
#include "plat.h"
#include "util.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "version.h"

#define DEFAULT_BASE "https://github.com/tofu-zyc/nano-agent/releases"
#define DEFAULT_TASK "nano-agent-kernel"
#define SYSTEMD_UNIT "nano-kernel.service"

typedef enum { SVC_NONE, SVC_SYSTEMD, SVC_LAUNCHD, SVC_TASK, SVC_STARTUP } svc_kind;

typedef struct {
    char    *conf;     /* kernel.conf */
    char    *dir;      /* 内核目录 */
    svc_kind svc;
    char     task[128];
    bool     quiet;
    bool     yes;
} ctl_t;

static char *in_dir(const ctl_t *c, const char *name) { return plat_join(c->dir, name); }

char *ctl_sidecar(const char *conf, const char *ext)
{
    const char *base = conf, *p;
    for (p = conf; *p; p++)
        if (*p == '/' || *p == '\\') base = p + 1;
    const char *dot = strrchr(base, '.');
    size_t stem = dot && dot != base ? (size_t)(dot - base) : strlen(base);
    return xasprintf("%.*s%s", (int)(base - conf + stem), conf, ext);
}

static bool exists(const char *p)
{
    plat_stat_t st;
    return p && plat_stat(p, &st) && st.exists;
}

static char *read_text(const char *p, size_t max)
{
    buf_t b = {0};
    if (plat_read_file(p, &b, max) < 0) {
        buf_free(&b);
        return NULL;
    }
    const char *t = b.len ? buf_cstr(&b) : "";
    if (!strncmp(t, "\xEF\xBB\xBF", 3)) t += 3;   /* 记事本存的 UTF-8 开头可能带 BOM */
    char *s = xstrdup(t);
    buf_free(&b);
    return s;
}

static char *trim(char *s)
{
    while (*s == ' ' || *s == '\t' || *s == '\r' || *s == '\n') s++;
    char *e = s + strlen(s);
    while (e > s && (e[-1] == ' ' || e[-1] == '\t' || e[-1] == '\r' || e[-1] == '\n')) *--e = 0;
    return s;
}

/* kernel.conf 里某一项的值（没有返回 NULL） */
static char *conf_get(const char *path, const char *key)
{
    char *text = read_text(path, 1 << 20);
    if (!text) return NULL;
    char *val = NULL;
    for (char *line = strtok(text, "\n"); line; line = strtok(NULL, "\n")) {
        char *l = trim(line);
        if (*l == '#') continue;
        char *eq = strchr(l, '=');
        if (!eq) continue;
        *eq = 0;
        if (!strcmp(trim(l), key)) {
            val = xstrdup(trim(eq + 1));
            break;
        }
    }
    free(text);
    return val;
}

/* 把粘在一起的行拆开（"max_output = 65536gateway = …"，以前的安装脚本少写了换行），其他原样保留 */
static char *unglue(const char *text)
{
    buf_t out = {0};
    for (const char *p = text; *p;) {
        const char *nl = strchr(p, '\n');
        size_t n = nl ? (size_t)(nl - p) : strlen(p);
        char *line = xmalloc(n + 1);
        memcpy(line, p, n);
        line[n] = 0;
        char *l = line;
        while (*l == ' ' || *l == '\t') l++;
        char *eq = *l != '#' ? strchr(l, '=') : NULL;
        const char *g;
        while (eq && (g = kconf_glued(eq + 1))) {
            buf_append(&out, l, (size_t)(g - l));
            buf_puts(&out, "\n");
            l = (char *)g;
            eq = strchr(l, '=');
        }
        buf_puts(&out, l);
        if (nl) buf_puts(&out, "\n");
        free(line);
        p += n + (nl ? 1 : 0);
    }
    char *s = xstrdup(out.len ? buf_cstr(&out) : "");
    buf_free(&out);
    return s;
}

/* 改 kernel.conf 里的一项（没有就加在最后），其他行原样保留 */
static int conf_set(const char *path, const char *key, const char *value)
{
    char *raw = read_text(path, 1 << 20);
    if (!raw) return -1;
    char *text = unglue(raw);
    free(raw);
    buf_t out = {0};
    bool done = false;
    char *p = text;
    while (*p) {
        char *nl = strchr(p, '\n');
        size_t n = nl ? (size_t)(nl - p) : strlen(p);
        char *line = xmalloc(n + 1);
        memcpy(line, p, n);
        line[n] = 0;
        char *l = trim(line);
        char *eq = *l != '#' ? strchr(l, '=') : NULL;
        bool hit = false;
        if (eq) {
            *eq = 0;
            hit = !strcmp(trim(l), key);
        }
        if (hit) {
            if (!done) {   /* 重复的同名项去掉，只留第一处 */
                buf_puts(&out, key);
                buf_puts(&out, " = ");
                buf_puts(&out, value);
                buf_puts(&out, "\n");
                done = true;
            }
        } else {
            buf_append(&out, p, n);
            if (nl) buf_puts(&out, "\n");
        }
        free(line);
        p += n + (nl ? 1 : 0);
    }
    if (!done) {
        if (out.len && out.p[out.len - 1] != '\n') buf_puts(&out, "\n");   /* 最后一行没有换行 */
        buf_puts(&out, key);
        buf_puts(&out, " = ");
        buf_puts(&out, value);
        buf_puts(&out, "\n");
    }
    int rc = plat_write_atomic(path, out.p ? out.p : "", out.len, 0600);
    buf_free(&out);
    free(text);
    return rc;
}

/* ---- 内核是怎么在后台运行的 ---- */

static bool systemd_ok(void)
{
    char *argv[] = { "systemctl", "--user", "show-environment", NULL };
    return plat_system(argv, true) == 0;
}

static char *systemd_unit_path(void)
{
    const char *xdg = getenv("XDG_CONFIG_HOME");
    char *home = plat_home();
    char *p = xdg && *xdg ? xasprintf("%s/systemd/user/%s", xdg, SYSTEMD_UNIT)
                          : xasprintf("%s/.config/systemd/user/%s", home ? home : "", SYSTEMD_UNIT);
    free(home);
    return p;
}

static char *launchd_plist(void)
{
    char *home = plat_home();
    char *p = xasprintf("%s/Library/LaunchAgents/com.nano-agent.kernel.plist", home ? home : "");
    free(home);
    return p;
}

static void detect_service(ctl_t *c)
{
    c->svc = SVC_NONE;
    snprintf(c->task, sizeof c->task, "%s", DEFAULT_TASK);
    char *f = in_dir(c, "service");
    char *s = read_text(f, 256);
    free(f);
    if (s) {
        char *v = trim(s);
        if (!strcmp(v, "systemd")) c->svc = SVC_SYSTEMD;
        else if (!strcmp(v, "launchd")) c->svc = SVC_LAUNCHD;
        else if (!strncmp(v, "task:", 5) && v[5]) {
            c->svc = SVC_TASK;
            snprintf(c->task, sizeof c->task, "%s", v + 5);
        } else if (!strncmp(v, "startup", 7)) {
            c->svc = SVC_STARTUP;
            if (v[7] == ':' && v[8]) snprintf(c->task, sizeof c->task, "%s", v + 8);
        }
        free(s);
        return;
    }
    /* 旧版本安装脚本没有写 service：按服务文件猜 */
    if (!strcmp(plat_os(), "windows")) {
        char *argv[] = { "schtasks.exe", "/Query", "/TN", (char *)DEFAULT_TASK, NULL };
        if (plat_system(argv, true) == 0) c->svc = SVC_TASK;
        return;
    }
    char *unit = systemd_unit_path(), *plist = launchd_plist();
    if (!strcmp(plat_os(), "macos") && exists(plist)) c->svc = SVC_LAUNCHD;
    else if (exists(unit) && systemd_ok()) c->svc = SVC_SYSTEMD;
    free(unit);
    free(plist);
}

static bool task_exists(const ctl_t *c)
{
    char *argv[] = { "schtasks.exe", "/Query", "/TN", (char *)c->task, NULL };
    return plat_system(argv, true) == 0;
}

static const char *svc_label(const ctl_t *c)
{
    switch (c->svc) {
    case SVC_SYSTEMD: return "systemd 用户服务";
    case SVC_LAUNCHD: return "launchd";
    case SVC_TASK:    return "Windows 任务计划";
    case SVC_STARTUP: return "Windows 启动文件夹";
    default:          return "后台进程";
    }
}

/* 正在运行的内核的 pid；没有返回 0 */
static long running_pid(const ctl_t *c)
{
    char *f = ctl_sidecar(c->conf, ".pid");
    char *s = read_text(f, 64);
    free(f);
    long pid = s ? strtol(trim(s), NULL, 10) : 0;
    free(s);
    return pid > 0 && plat_pid_alive(pid, "nano-kernel") ? pid : 0;
}

static cJSON *read_status(const ctl_t *c)
{
    char *f = ctl_sidecar(c->conf, ".status.json");
    char *s = read_text(f, 1 << 16);
    free(f);
    cJSON *j = s ? cJSON_Parse(s) : NULL;
    free(s);
    return j;
}

static const char *jstr(const cJSON *o, const char *k)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(o, k);
    return cJSON_IsString(v) ? v->valuestring : NULL;
}

static void print_duration(long secs)
{
    if (secs < 60) printf("%ld 秒", secs);
    else if (secs < 3600) printf("%ld 分钟", secs / 60);
    else if (secs < 86400) printf("%ld 小时 %ld 分", secs / 3600, secs / 60 % 60);
    else printf("%ld 天 %ld 小时", secs / 86400, secs / 3600 % 24);
}

/* 一行连接状态；返回 state（调用方 free），没在运行或没有状态返回 NULL */
static char *print_connection(const ctl_t *c, long pid)
{
    char *gw = conf_get(c->conf, "gateway");
    printf("网关：%s", gw ? gw : "（没有配置）");
    free(gw);
    if (!pid) {
        printf("\n");
        return NULL;
    }
    cJSON *st = read_status(c);
    const cJSON *spid = cJSON_GetObjectItemCaseSensitive(st, "pid");
    const char *state = jstr(st, "state");
    if (!st || !state || !cJSON_IsNumber(spid) || (long)spid->valuedouble != pid) {
        printf(" · 正在启动\n");
        cJSON_Delete(st);
        return NULL;
    }
    const char *detail = jstr(st, "detail");
    if (!strcmp(state, "connected")) {
        const cJSON *since = cJSON_GetObjectItemCaseSensitive(st, "since");
        printf(" · 已连接");
        if (cJSON_IsNumber(since)) {
            printf("（");
            print_duration((long)(time(NULL) - (time_t)since->valuedouble));
            printf("）");
        }
    } else if (!strcmp(state, "pairing")) {
        const char *code = jstr(st, "code");
        printf(" · 等网关确认接入：在网关网页的「设备」里核对确认码 %.3s %s，点允许", code ? code : "?",
               code && strlen(code) > 3 ? code + 3 : "");
    } else if (!strcmp(state, "denied")) {
        printf(" · 网关没有同意这台电脑接入（重新申请：nano-kernel restart）");
    } else if (!strcmp(state, "rejected")) {
        printf(" · 网关拒绝连接：%s", detail ? detail : "");
    } else {
        printf(" · 正在连接");
        if (detail && *detail) printf("（%s）", detail);
    }
    printf("\n");
    char *out = xstrdup(state);
    cJSON_Delete(st);
    return out;
}

/* ---- 命令 ---- */

static int cmd_status(ctl_t *c)
{
    char *node = conf_get(c->conf, "node");
    printf("nano-kernel %s · 这台电脑：%s\n", NA_VERSION, node ? node : "（没有配置）");
    free(node);
    long pid = running_pid(c);
    if (pid) printf("运行：运行中（pid %ld，%s）\n", pid, svc_label(c));
    else printf("运行：没有运行（nano-kernel start 启动）\n");
    if (c->svc == SVC_TASK && !task_exists(c))
        printf("自启：任务计划里没有 %s，登录后不会自动启动（重新运行一次安装命令修好）\n", c->task);
    free(print_connection(c, pid));
    cJSON *st = pid ? read_status(c) : NULL;
    const cJSON *ll = cJSON_GetObjectItemCaseSensitive(st, "landlock");
    if (cJSON_IsNumber(ll)) {
        if (ll->valuedouble > 0) printf("沙箱：Landlock v%d\n", (int)ll->valuedouble);
        else printf("沙箱：没有（自动模式下的 Shell 也要审批）\n");
    }
    cJSON_Delete(st);
    char *log = in_dir(c, "kernel.log");
    printf("配置：%s\n日志：%s\n", c->conf, log);
    free(log);
    return pid ? 0 : 3;
}

static long wait_running(const ctl_t *c, int ms)
{
    for (int i = 0; i < ms / 100; i++) {
        long pid = running_pid(c);
        if (pid) return pid;
        plat_sleep_ms(100);
    }
    return 0;
}

static bool wait_stopped(long pid, int ms)
{
    for (int i = 0; i < ms / 100; i++) {
        if (!plat_pid_alive(pid, "nano-kernel")) return true;
        plat_sleep_ms(100);
    }
    return !plat_pid_alive(pid, "nano-kernel");
}

/* 等连接有个结果（连上、等确认、被拒），最多 ms 毫秒，然后打印 */
static void wait_connection(const ctl_t *c, int ms)
{
    long pid = 0;
    for (int i = 0; i < ms / 200; i++) {
        pid = running_pid(c);
        cJSON *st = pid ? read_status(c) : NULL;
        const char *state = jstr(st, "state");
        const cJSON *spid = cJSON_GetObjectItemCaseSensitive(st, "pid");
        bool settled = state && cJSON_IsNumber(spid) && (long)spid->valuedouble == pid && strcmp(state, "connecting") != 0;
        cJSON_Delete(st);
        if (settled) break;
        plat_sleep_ms(200);
    }
    free(print_connection(c, pid));
}

static int cmd_start(ctl_t *c)
{
    char *stop = ctl_sidecar(c->conf, ".stop");
    plat_unlink(stop);   /* 上次没被处理的停止请求作废 */
    free(stop);
    long pid = running_pid(c);
    if (pid) {
        if (!c->quiet) printf("内核已经在运行（pid %ld）\n", pid);
        return 0;
    }
    int rc = 0;
    if (c->svc == SVC_SYSTEMD) {
        char *argv[] = { "systemctl", "--user", "start", SYSTEMD_UNIT, NULL };
        rc = plat_system(argv, false);
    } else if (c->svc == SVC_LAUNCHD) {
        char *plist = launchd_plist();
        char *argv[] = { "launchctl", "load", "-w", plist, NULL };
        rc = plat_system(argv, false);
        free(plist);
    }
    bool direct = c->svc == SVC_NONE || c->svc == SVC_STARTUP;
    if (c->svc == SVC_TASK) {
        char *argv[] = { "schtasks.exe", "/Run", "/TN", c->task, NULL };
        if (plat_system(argv, true) != 0) {
            /* 任务没建成（比如以前的安装脚本用普通用户建不了）：不靠它，直接拉起 run-kernel.ps1 */
            if (!c->quiet) printf("任务计划里没有 %s（登录后不会自动启动，重新运行安装命令修好），这次直接启动\n", c->task);
            direct = true;
        }
    }
    if (direct) {
        char *runner = in_dir(c, "run-kernel.ps1");
        char *log = in_dir(c, "kernel.log");
        char *self = plat_join(c->dir, !strcmp(plat_os(), "windows") ? "nano-kernel.exe" : "nano-kernel");
        long child;
        if (!strcmp(plat_os(), "windows") && exists(runner)) {
            /* Windows：交给 run-kernel.ps1，它在内核更新或崩溃后会重新拉起 */
            char *argv[] = { "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden",
                             "-File", runner, NULL };
            child = plat_spawn_bg(argv, NULL);
        } else {
            char *argv[] = { self, "-c", c->conf, "--log", log, NULL };
            child = plat_spawn_bg(argv, NULL);
        }
        rc = child > 0 ? 0 : -1;
        free(runner);
        free(log);
        free(self);
    }
    if (rc != 0) {
        fprintf(stderr, "启动失败（%s）\n", svc_label(c));
        return 1;
    }
    pid = wait_running(c, 8000);
    if (!pid) {
        char *log = in_dir(c, "kernel.log");
        fprintf(stderr, "内核没有起来，看看日志：%s\n", log);
        free(log);
        return 1;
    }
    if (!c->quiet) {
        printf("内核已启动（pid %ld，%s）\n", pid, svc_label(c));
        wait_connection(c, 6000);
    }
    return 0;
}

static int cmd_stop(ctl_t *c)
{
    long pid = running_pid(c);
    if (c->svc == SVC_SYSTEMD) {
        char *argv[] = { "systemctl", "--user", "stop", SYSTEMD_UNIT, NULL };
        if (plat_system(argv, false) != 0) return 1;
    } else if (c->svc == SVC_LAUNCHD) {
        char *plist = launchd_plist();
        char *argv[] = { "launchctl", "unload", plist, NULL };
        plat_system(argv, true);
        free(plist);
    }
    if (!pid) {
        if (!c->quiet) printf("内核没有在运行\n");
        return 0;
    }
    if (c->svc == SVC_NONE || c->svc == SVC_TASK || c->svc == SVC_STARTUP) {
        if (!strcmp(plat_os(), "windows")) {
            /* Windows 没有 SIGTERM：写 stop 文件请内核自己正常退出，run-kernel.ps1 看到退出码 0 就不再拉起 */
            char *stop = ctl_sidecar(c->conf, ".stop");
            plat_write_atomic(stop, "stop\n", 5, 0600);
            free(stop);
        } else {
            plat_pid_kill(pid, false);
        }
    }
    if (!wait_stopped(pid, 8000)) {
        if (c->svc == SVC_TASK) {
            char *argv[] = { "schtasks.exe", "/End", "/TN", c->task, NULL };
            plat_system(argv, true);
        }
        plat_pid_kill(pid, true);
        if (!wait_stopped(pid, 3000)) {
            fprintf(stderr, "停不下来（pid %ld）\n", pid);
            return 1;
        }
    }
    if (!c->quiet) printf("内核已停止\n");
    return 0;
}

static int cmd_restart(ctl_t *c)
{
    if (c->svc == SVC_SYSTEMD) {
        char *argv[] = { "systemctl", "--user", "restart", SYSTEMD_UNIT, NULL };
        if (plat_system(argv, false) != 0) return 1;
        long pid = wait_running(c, 8000);
        if (!pid) return 1;
        printf("内核已重启（pid %ld，%s）\n", pid, svc_label(c));
        wait_connection(c, 6000);
        return 0;
    }
    bool quiet = c->quiet;
    c->quiet = true;
    int rc = cmd_stop(c);
    c->quiet = quiet;
    return rc ? rc : cmd_start(c);
}

/* 网关地址：host 补默认端口 7700，host:port 补 tcp: */
static char *normalize_gateway(const char *a)
{
    if (!strncmp(a, "unix:", 5) || !strncmp(a, "tcp:", 4)) return xstrdup(a);
    if (strchr(a, ':')) return xasprintf("tcp:%s", a);
    return xasprintf("tcp:%s:7700", a);
}

static bool valid_name(const char *n)
{
    if (!n || !*n || strlen(n) > 64) return false;
    if (!strcmp(n, "本机") || !strcmp(n, "local") || !strcmp(n, "LOCAL")) return false;
    for (const char *p = n; *p; p++)
        if (*p == ' ' || *p == '\t' || *p == ':' || *p == '/' || *p == '\\') return false;
    return true;
}

static int cmd_connect(ctl_t *c, int argc, char **argv)
{
    const char *addr = NULL, *name = NULL;
    for (int i = 0; i < argc; i++) {
        if (!strcmp(argv[i], "--name") && i + 1 < argc) name = argv[++i];
        else if (!strncmp(argv[i], "--name=", 7)) name = argv[i] + 7;
        else if (!addr && argv[i][0] != '-') addr = argv[i];
        else {
            fprintf(stderr, "用法：nano-kernel connect 网关地址 [--name 这台电脑的名字]\n");
            return 2;
        }
    }
    if (!addr && !name) {
        char *gw = conf_get(c->conf, "gateway");
        printf("现在连的网关：%s\n改：nano-kernel connect 网关地址（比如 192.168.1.2:7700）\n", gw ? gw : "（没有配置）");
        free(gw);
        return 0;
    }
    if (name && !valid_name(name)) {
        fprintf(stderr, "名字不能为空，不能有空格、冒号和斜杠，也不能叫“本机”：%s\n", name);
        return 2;
    }
    if (addr) {
        char *old = conf_get(c->conf, "gateway");
        char *gw = normalize_gateway(addr);
        if (conf_set(c->conf, "gateway", gw) < 0) {
            fprintf(stderr, "改不了 %s：%s\n", c->conf, strerror(errno));
            free(gw);
            free(old);
            return 1;
        }
        printf("网关地址：%s → %s\n", old ? old : "（没有）", gw);
        if (old && strcmp(old, gw) != 0)
            printf("换了网关的话，新网关不认这台电脑原来的配对口令，内核会自动重新申请接入，到新网关的网页里点允许就行\n");
        free(gw);
        free(old);
    }
    if (name) {
        if (conf_set(c->conf, "node", name) < 0) {
            fprintf(stderr, "改不了 %s：%s\n", c->conf, strerror(errno));
            return 1;
        }
        printf("这台电脑的名字：%s\n", name);
    }
    if (!running_pid(c) && c->svc == SVC_NONE) {
        printf("内核没有在运行，nano-kernel start 启动\n");
        return 0;
    }
    return cmd_restart(c);
}

static int cmd_logs(ctl_t *c, int argc, char **argv)
{
    bool follow = false;
    long n = 50;
    for (int i = 0; i < argc; i++) {
        if (!strcmp(argv[i], "-f") || !strcmp(argv[i], "--follow")) follow = true;
        else if (!strcmp(argv[i], "-n") && i + 1 < argc) n = strtol(argv[++i], NULL, 10);
        else {
            fprintf(stderr, "用法：nano-kernel logs [-f] [-n 行数]\n");
            return 2;
        }
    }
    char *log = in_dir(c, "kernel.log");
    buf_t b = {0};
    if (plat_read_file(log, &b, 64u << 20) < 0) {
        fprintf(stderr, "还没有日志：%s\n", log);
        free(log);
        return 1;
    }
    size_t start = b.len;
    for (long seen = 0; start > 0; start--) {
        if (b.p[start - 1] == '\n' && start != b.len && ++seen >= n) break;
    }
    fwrite(b.p + start, 1, b.len - start, stdout);
    fflush(stdout);
    size_t off = b.len;
    buf_free(&b);
    while (follow) {
        plat_sleep_ms(500);
        plat_stat_t st;
        if (!plat_stat(log, &st) || !st.exists || (size_t)st.size == off) continue;
        plat_file *f = plat_open_read(log);
        if (!f) continue;
        char chunk[8192];
        size_t pos = 0;
        long r;
        bool shrank = true;
        while ((r = plat_file_read(f, chunk, sizeof chunk)) > 0) {
            size_t end = pos + (size_t)r;
            if (end > off) {
                size_t from = pos < off ? off - pos : 0;
                fwrite(chunk + from, 1, (size_t)r - from, stdout);
            }
            pos = end;
            shrank = false;
        }
        plat_file_close(f);
        if (pos < off && !shrank) off = 0;   /* 日志被轮换了，从头来 */
        else off = pos > off ? pos : off;
        fflush(stdout);
    }
    free(log);
    return 0;
}

static int cmd_update(ctl_t *c, int argc, char **argv)
{
    for (int i = 0; i < argc; i++) {
        if (!strcmp(argv[i], "--version") && i + 1 < argc) i++;
        else {
            fprintf(stderr, "用法：nano-kernel update [--version x.y.z]\n");
            return 2;
        }
    }
    const char *base = getenv("NANO_DOWNLOAD_URL");
    if (!base || !*base) base = DEFAULT_BASE;
    if (!strcmp(plat_os(), "windows")) {
        char *gw = conf_get(c->conf, "gateway"), *node = conf_get(c->conf, "node");
        buf_t cmd = {0};
        buf_puts(&cmd, "& ([scriptblock]::Create((irm '");
        buf_puts(&cmd, base);
        buf_puts(&cmd, "/latest/download/install.ps1'))) -Yes -Dir '");
        buf_puts(&cmd, c->dir);
        buf_puts(&cmd, "'");
        if (gw) { buf_puts(&cmd, " -Gateway '"); buf_puts(&cmd, gw); buf_puts(&cmd, "'"); }
        if (node) { buf_puts(&cmd, " -Name '"); buf_puts(&cmd, node); buf_puts(&cmd, "'"); }
        if (c->svc == SVC_TASK || c->svc == SVC_STARTUP) { buf_puts(&cmd, " -TaskName '"); buf_puts(&cmd, c->task); buf_puts(&cmd, "'"); }
        if (c->svc == SVC_STARTUP) buf_puts(&cmd, " -NoTask");
        if (c->svc == SVC_NONE) buf_puts(&cmd, " -NoService");
        for (int i = 0; i + 1 < argc; i++)
            if (!strcmp(argv[i], "--version")) { buf_puts(&cmd, " -Version '"); buf_puts(&cmd, argv[i + 1]); buf_puts(&cmd, "'"); }
        char *pargv[] = { "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", buf_cstr(&cmd), NULL };
        printf("更新内核（安装脚本会先停掉内核，这个命令窗口里的输出可能中断，装好后用 nano-kernel status 看）…\n");
        fflush(stdout);
        int rc = plat_system(pargv, false);
        buf_free(&cmd);
        free(gw);
        free(node);
        return rc == 0 ? 0 : 1;
    }
    static const char *script =
        "base=$1; dir=$2; shift 2\n"
        "if command -v curl >/dev/null 2>&1; then get() { curl -fsSL --retry 3 \"$1\"; }; else get() { wget -qO- \"$1\"; }; fi\n"
        "case \" $* \" in *\" --version \"*) ;; *)\n"
        "  latest=$(get \"$base/latest/download/VERSION\" | tr -d ' \\r\\n') || { echo \"查不到最新版本（$base）\"; exit 1; }\n"
        "  cur=$(\"$dir/nano-kernel\" --version 2>/dev/null)\n"
        "  if [ \"$latest\" = \"$cur\" ]; then echo \"已经是最新版本 $cur\"; exit 0; fi\n"
        "  echo \"更新内核：$cur → $latest\" ;; esac\n"
        "script=$(get \"$base/latest/download/install.sh\") || { echo \"下载安装脚本失败（$base）\"; exit 1; }\n"
        "printf '%s\\n' \"$script\" | sh -s -- --kernel --yes --dir \"$dir\" \"$@\"\n";
    char **sargv = xcalloc((size_t)argc + 7, sizeof *sargv);
    int k = 0;
    sargv[k++] = "sh";
    sargv[k++] = "-c";
    sargv[k++] = (char *)script;
    sargv[k++] = "nano-kernel-update";
    sargv[k++] = (char *)base;
    sargv[k++] = c->dir;
    for (int i = 0; i < argc; i++) sargv[k++] = argv[i];
    sargv[k] = NULL;
    int rc = plat_system(sargv, false);
    free(sargv);
    return rc == 0 ? 0 : 1;
}

static int cmd_uninstall(ctl_t *c)
{
    if (!strcmp(plat_os(), "windows")) {
        char *ps1 = in_dir(c, "install-kernel.ps1");
        if (!exists(ps1)) {
            fprintf(stderr, "内核目录里没有 install-kernel.ps1；用一行安装命令把 -Gateway … 换成 -Uninstall\n");
            free(ps1);
            return 1;
        }
        char *argv[] = { "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", ps1, "-Uninstall",
                         "-Dir", c->dir, "-TaskName", c->task, c->yes ? "-Yes" : NULL, NULL };
        int rc = plat_system(argv, false);
        free(ps1);
        return rc == 0 ? 0 : 1;
    }
    char *sh = in_dir(c, "install-kernel.sh");
    if (!exists(sh)) {
        fprintf(stderr, "内核目录里没有 install-kernel.sh；用一行安装命令重装一次，或者手动删掉 %s\n", c->dir);
        free(sh);
        return 1;
    }
    char *argv[] = { "sh", sh, "--uninstall", "--dir", c->dir, c->yes ? "--yes" : NULL, NULL };
    int rc = plat_system(argv, false);
    free(sh);
    return rc == 0 ? 0 : 1;
}

/* ---- 入口 ---- */

static const char *const COMMANDS[] = { "status", "start", "stop", "restart", "connect", "logs", "log",
                                         "version", "update", "uninstall", "help", NULL };

/* 单个命令的帮助：nano-kernel 命令 --help、nano-kernel help 命令 */
static const struct {
    const char *name, *usage, *text;
} HELP[] = {
    { "status", "nano-kernel status",
      "运行状态：在不在跑、怎么跑的（systemd / launchd / 任务计划 / 后台进程）、和网关的连接\n"
      "（已连接多久 / 正在连接和连不上的原因 / 等网关确认时的确认码 / 被拒绝）、版本、沙箱。\n"
      "没有在运行时退出码是 3。" },
    { "start", "nano-kernel start [--quiet]",
      "在后台启动：装了服务（systemd / launchd / 任务计划 / 启动文件夹）就照它的方式启动，否则作为后台进程运行。\n"
      "启动后等几秒，显示和网关的连接情况。\n"
      "  --quiet  不打印提示" },
    { "stop", "nano-kernel stop [--quiet]",
      "停止。下次开机（登录）时还会自动启动。\n"
      "  --quiet  不打印提示" },
    { "restart", "nano-kernel restart",
      "重启（手动改了 kernel.conf 之后用）。" },
    { "connect", "nano-kernel connect [网关地址] [--name 名字]",
      "改连哪个网关、改这台电脑的名字，然后重启。\n"
      "  网关地址     192.168.1.2、192.168.1.2:7700、tcp:主机:端口 或 unix:/路径（只写主机时端口是 7700）\n"
      "  --name 名字  这台电脑在网关里显示的名字\n"
      "不带参数时显示现在连的网关。\n"
      "换了网关的话，新网关不认原来的配对口令：内核会自动重新申请接入，到新网关网页的「设备」里点允许。" },
    { "logs", "nano-kernel logs [-f] [-n 行数]",
      "看日志。\n"
      "  -f       持续输出新日志（Ctrl-C 退出）\n"
      "  -n 行数  显示最后几行，默认 50" },
    { "version", "nano-kernel version", "版本号。" },
    { "update", "nano-kernel update [--version x.y.z]",
      "下载新版本重新安装，配置和配对口令保留。\n"
      "一般用不着：连上网关后，内核会自动换成和网关一样的版本。\n"
      "  --version x.y.z  装指定版本" },
    { "uninstall", "nano-kernel uninstall [--yes]",
      "停止并删除内核（包括配置和配对口令），去掉开机自启和 PATH。\n"
      "  --yes  不提问" },
    { NULL, NULL, NULL },
};

static int command_help(const char *name)
{
    if (!strcmp(name, "log")) name = "logs";
    for (int i = 0; HELP[i].name; i++) {
        if (!strcmp(HELP[i].name, name)) {
            printf("用法：%s\n\n%s\n  -c 配置文件  默认是程序旁边的 kernel.conf\n", HELP[i].usage, HELP[i].text);
            return 0;
        }
    }
    ctl_usage(stdout);
    return 0;
}

static bool is_help_flag(const char *a)
{
    return !strcmp(a, "-h") || !strcmp(a, "--help") || !strcmp(a, "-help");
}

bool ctl_is_command(const char *s)
{
    for (int i = 0; s && COMMANDS[i]; i++)
        if (!strcmp(s, COMMANDS[i])) return true;
    return false;
}

void ctl_usage(FILE *out)
{
    fprintf(out,
            "nano-kernel：nano-agent 的内核\n"
            "\n"
            "管理（装好后在任何目录都能用）：\n"
            "  nano-kernel status                    运行状态、和网关的连接、版本\n"
            "  nano-kernel start | stop | restart\n"
            "  nano-kernel connect 地址 [--name 名字]  改连哪个网关（比如 192.168.1.2:7700），然后重启\n"
            "  nano-kernel logs [-f] [-n 行数]\n"
            "  nano-kernel version\n"
            "  nano-kernel update [--version x.y.z]  手动更新（一般不用：连上网关会自动换成网关的版本）\n"
            "  nano-kernel uninstall [--yes]\n"
            "  以上都可以加 -c kernel.conf 指定配置（默认是程序旁边的 kernel.conf）\n"
            "  nano-kernel help 命令（或 nano-kernel 命令 --help）看单个命令的用法\n"
            "\n"
            "运行（服务管理器用）：\n"
            "  nano-kernel -c kernel.conf [-v] [--log 文件]  在前台运行（--log：日志追加写到文件）\n"
            "  nano-kernel -c kernel.conf -t         只检查配置和执行器\n");
}

int ctl_main(int argc, char **argv)
{
    const char *cmd = argv[1];
    if (!strcmp(cmd, "help")) {
        if (argc > 2) return command_help(argv[2]);
        ctl_usage(stdout);
        return 0;
    }
    for (int i = 2; i < argc; i++)
        if (is_help_flag(argv[i])) return command_help(cmd);
    if (!strcmp(cmd, "version")) {
        printf("%s\n", NA_VERSION);
        return 0;
    }
    ctl_t c = {0};
    /* 公共选项：-c、--quiet、--yes；剩下的交给子命令 */
    char **rest = xcalloc((size_t)argc + 1, sizeof *rest);
    int nrest = 0;
    for (int i = 2; i < argc; i++) {
        if (!strcmp(argv[i], "-c") && i + 1 < argc) c.conf = xstrdup(argv[++i]);
        else if (!strcmp(argv[i], "--quiet") || !strcmp(argv[i], "-q")) c.quiet = true;
        else if (!strcmp(argv[i], "--yes") || !strcmp(argv[i], "-y")) c.yes = true;
        else rest[nrest++] = argv[i];
    }
    if (nrest > 0 && strcmp(cmd, "connect") != 0 && strcmp(cmd, "logs") != 0 && strcmp(cmd, "log") != 0 &&
        strcmp(cmd, "update") != 0) {
        fprintf(stderr, "%s 不认识的参数：%s（nano-kernel %s --help 看用法）\n", cmd, rest[0], cmd);
        free(rest);
        free(c.conf);
        return 2;
    }
    if (!c.conf) {
        char *d = plat_exe_dir();
        c.conf = plat_join(d ? d : ".", "kernel.conf");
        free(d);
    } else {
        char *abs = plat_abspath(NULL, c.conf);
        if (abs) {
            free(c.conf);
            c.conf = abs;
        }
    }
    c.dir = plat_dirname(c.conf);
    int rc;
    if (!exists(c.conf) && strcmp(cmd, "uninstall") != 0) {
        fprintf(stderr, "找不到内核配置 %s（内核还没装？用一行安装命令装，或者用 -c 指定配置）\n", c.conf);
        rc = 1;
        goto out;
    }
    detect_service(&c);
    if (!strcmp(cmd, "status")) rc = cmd_status(&c);
    else if (!strcmp(cmd, "start")) rc = cmd_start(&c);
    else if (!strcmp(cmd, "stop")) rc = cmd_stop(&c);
    else if (!strcmp(cmd, "restart")) rc = cmd_restart(&c);
    else if (!strcmp(cmd, "connect")) rc = cmd_connect(&c, nrest, rest);
    else if (!strcmp(cmd, "logs") || !strcmp(cmd, "log")) rc = cmd_logs(&c, nrest, rest);
    else if (!strcmp(cmd, "update")) rc = cmd_update(&c, nrest, rest);
    else rc = cmd_uninstall(&c);
out:
    free(rest);
    free(c.conf);
    free(c.dir);
    return rc;
}
