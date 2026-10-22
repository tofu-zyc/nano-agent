/* nexec_proc.c —— 子进程执行，以及 Shell / Grep
 *
 * 命令跑在自己的进程组里：超时、取消时整组杀掉，命令结束后残留的后台进程也一并清理。
 * 取消信号有两个来源：stdin 被关闭（内核或 ssh 连接没了），或者收到 SIGTERM（内核要求停止）。
 */
#include "nexec.h"

#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define LINGER_MS 2000   /* 命令退出后，最多再等这么久让占着输出管道的后台进程放手 */

static int g_sig[2] = { -1, -1 };

static void on_sig(int sig)
{
    int saved = errno;
    char c = (char)sig;
    if (write(g_sig[1], &c, 1) < 0) { /* 管道满了也无妨 */ }
    errno = saved;
}

/* ---- 输出：保留开头和结尾，省略中间 ---- */

typedef struct {
    buf_t  head, tail;
    size_t head_max, tail_max, total;
} capture;

static void cap_add(capture *c, const char *p, size_t n)
{
    c->total += n;
    if (c->head.len < c->head_max) {
        size_t take = c->head_max - c->head.len;
        if (take > n) take = n;
        buf_append(&c->head, p, take);
        p += take;
        n -= take;
    }
    if (!n) return;
    buf_append(&c->tail, p, n);
    if (c->tail.len > 2 * c->tail_max) buf_consume(&c->tail, c->tail.len - c->tail_max);
}

static void cap_render(capture *c, buf_t *out)
{
    const char *h = c->head.p ? c->head.p : "";
    size_t skip = c->tail.len > c->tail_max ? c->tail.len - c->tail_max : 0;
    const char *t = c->tail.p ? c->tail.p + skip : "";
    size_t tl = c->tail.len - skip;
    size_t omitted = c->total - c->head.len - tl;
    if (!omitted) {
        /* 开头和结尾是连着的，合在一起处理，避免在接缝处切坏 UTF-8 字符 */
        buf_t all = {0};
        buf_append(&all, h, c->head.len);
        buf_append(&all, t, tl);
        buf_put_utf8(out, buf_cstr(&all), all.len);
        buf_free(&all);
        return;
    }
    buf_put_utf8(out, h, utf8_cut(h, c->head.len, c->head.len));
    while (tl && ((unsigned char)*t & 0xC0) == 0x80) { t++; tl--; }   /* 从完整的字符开始 */
    char note[96];
    snprintf(note, sizeof note, "\n…（中间省略 %zu 字节）…\n", omitted);
    buf_puts(out, note);
    buf_put_utf8(out, t, tl);
}

/* ---- 执行 ---- */

char *find_in_path(const xctx *x, const char *name)
{
    const char *path = NULL;
    for (char **e = x->env; *e; e++)
        if (!strncmp(*e, "PATH=", 5)) path = *e + 5;
    if (!path) path = "/usr/local/bin:/usr/bin:/bin";
    char *dup = xstrdup(path), *save = NULL;
    for (char *d = strtok_r(dup, ":", &save); d; d = strtok_r(NULL, ":", &save)) {
        char *full = xasprintf("%s/%s", d, name);
        if (access(full, X_OK) == 0) {
            free(dup);
            return full;
        }
        free(full);
    }
    free(dup);
    return NULL;
}

static void kill_group(pid_t pg)
{
    kill(-pg, SIGKILL);
}

int run_proc(const xctx *x, char *const argv[], const char *cwd, int timeout_s, buf_t *out, proc_status *st)
{
    memset(st, 0, sizeof *st);
    int p[2];
    if (pipe(p) < 0) {
        buf_puts(out, "创建管道失败：");
        buf_puts(out, strerror(errno));
        return -1;
    }
    set_cloexec(p[0]);
    set_cloexec(p[1]);
    if (g_sig[0] < 0) {
        if (pipe(g_sig) < 0) { close(p[0]); close(p[1]); return -1; }
        set_nonblock(g_sig[0]); set_nonblock(g_sig[1]);
        set_cloexec(g_sig[0]); set_cloexec(g_sig[1]);
        struct sigaction sa;
        memset(&sa, 0, sizeof sa);
        sa.sa_handler = on_sig;
        sigemptyset(&sa.sa_mask);
        sa.sa_flags = SA_RESTART | SA_NOCLDSTOP;
        sigaction(SIGCHLD, &sa, NULL);
        sigaction(SIGTERM, &sa, NULL);
        sigaction(SIGHUP, &sa, NULL);
    }

    pid_t parent = getpid();
    pid_t pid = fork();
    if (pid < 0) {
        buf_puts(out, "fork 失败：");
        buf_puts(out, strerror(errno));
        close(p[0]); close(p[1]);
        return -1;
    }
    if (pid == 0) {
        setpgid(0, 0);
        prctl(PR_SET_PDEATHSIG, SIGKILL);   /* 执行器被强杀时 shell 也跟着死 */
        if (getppid() != parent) _exit(127);
        signal(SIGCHLD, SIG_DFL);
        signal(SIGTERM, SIG_DFL);
        signal(SIGHUP, SIG_DFL);
        signal(SIGPIPE, SIG_DFL);
        int dn = open("/dev/null", O_RDONLY);
        if (dn < 0 || dup2(dn, 0) < 0 || dup2(p[1], 1) < 0 || dup2(p[1], 2) < 0) _exit(127);
        if (chdir(cwd) < 0) {
            dprintf(2, "进入目录 %s 失败：%s\n", cwd, strerror(errno));
            _exit(127);
        }
        execve(argv[0], argv, x->env);
        dprintf(2, "无法执行 %s：%s\n", argv[0], strerror(errno));
        _exit(127);
    }
    setpgid(pid, pid);
    close(p[1]);
    set_nonblock(p[0]);

    capture c = { .head_max = x->max_output * 3 / 4, .tail_max = x->max_output / 4 };
    int64_t deadline = now_ms() + (int64_t)timeout_s * 1000, linger = -1;
    bool exited = false, open_out = true, cancelled = false;
    char tmp[16384];

    while (open_out || !exited) {
        int64_t now = now_ms();
        if (!exited && now >= deadline) {
            st->timed_out = true;
            break;
        }
        if (exited && now >= linger) break;
        int64_t until = exited ? linger : deadline;
        struct pollfd pf[3] = {
            { .fd = g_sig[0], .events = POLLIN },
            { .fd = open_out ? p[0] : -1, .events = POLLIN },
            { .fd = x->watch_stdin ? 0 : -1, .events = POLLIN },
        };
        int wait = (int)(until - now);
        if (wait > 1000) wait = 1000;
        if (poll(pf, 3, wait) < 0 && errno != EINTR) break;

        if (pf[0].revents & POLLIN) {
            char s[16];
            ssize_t n;
            while ((n = read(g_sig[0], s, sizeof s)) > 0)
                for (ssize_t i = 0; i < n; i++)
                    if (s[i] == SIGTERM || s[i] == SIGHUP) cancelled = true;
        }
        if (pf[2].revents & (POLLIN | POLLHUP | POLLERR)) {
            ssize_t n = read(0, tmp, sizeof tmp);
            if (n <= 0 && !(n < 0 && (errno == EINTR || errno == EAGAIN))) cancelled = true;
        }
        if (cancelled) {
            /* 内核已经不要结果了：杀掉命令直接退出，不再输出 */
            kill_group(pid);
            _exit(2);
        }
        if (pf[1].revents & (POLLIN | POLLHUP | POLLERR)) {
            for (int i = 0; i < 16; i++) {
                ssize_t n = read(p[0], tmp, sizeof tmp);
                if (n > 0) { cap_add(&c, tmp, (size_t)n); continue; }
                if (n < 0 && errno == EINTR) continue;
                if (n < 0 && errno == EAGAIN) break;
                open_out = false;
                break;
            }
        }
        if (!exited && waitpid(pid, &st->status, WNOHANG) == pid) {
            exited = true;
            linger = now_ms() + LINGER_MS;
        }
    }

    kill_group(pid);   /* 超时、取消，或者命令留下的后台进程 */
    if (!exited) waitpid(pid, &st->status, 0);
    close(p[0]);
    cap_render(&c, out);
    buf_free(&c.head);
    buf_free(&c.tail);
    return 0;
}

static void put_status(buf_t *out, const proc_status *st, int timeout_s)
{
    char note[96] = "";
    if (st->timed_out) snprintf(note, sizeof note, "（执行超时（%d 秒），已终止）", timeout_s);
    else if (WIFEXITED(st->status) && WEXITSTATUS(st->status)) snprintf(note, sizeof note, "（退出码 %d）", WEXITSTATUS(st->status));
    else if (WIFSIGNALED(st->status)) snprintf(note, sizeof note, "（被信号 %d 终止）", WTERMSIG(st->status));
    if (!*note) return;
    if (out->len && out->p[out->len - 1] != '\n') buf_puts(out, "\n");
    buf_puts(out, note);
}

/* ---- Shell ---- */

bool tool_shell(const xctx *x, const cJSON *a, buf_t *out)
{
    const cJSON *cmd = cJSON_GetObjectItemCaseSensitive(a, "command");
    const cJSON *to = cJSON_GetObjectItemCaseSensitive(a, "timeout");
    if (!cJSON_IsString(cmd) || !*cmd->valuestring) {
        buf_puts(out, "缺少 command（字符串）");
        return false;
    }
    if (!x->ndirs && !x->home_only) {
        buf_puts(out, "还没有设置工作目录，Shell 没有地方运行。请让用户先用 /dir add <路径> 添加工作目录");
        return false;
    }
    int timeout = cJSON_IsNumber(to) ? (int)to->valuedouble : SHELL_DEFAULT_TIMEOUT;
    if (timeout < 1) timeout = 1;
    if (timeout > SHELL_MAX_TIMEOUT) timeout = SHELL_MAX_TIMEOUT;

    char *argv[] = { "/bin/sh", "-c", cmd->valuestring, NULL };
    proc_status st;
    if (run_proc(x, argv, x->ndirs ? x->dirs[0] : x->home, timeout, out, &st) < 0) return false;
    if (!out->len) buf_puts(out, "（无输出）");
    put_status(out, &st, timeout);
    return !st.timed_out && WIFEXITED(st.status) && WEXITSTATUS(st.status) == 0;
}

/* ---- Grep ---- */

/* 有 ripgrep 就用它（自动跳过 .gitignore 里的文件），否则退回 grep / find */
bool tool_grep(const xctx *x, const cJSON *a, buf_t *out)
{
    const cJSON *pv = cJSON_GetObjectItemCaseSensitive(a, "pattern");
    const cJSON *gv = cJSON_GetObjectItemCaseSensitive(a, "glob");
    const cJSON *pathv = cJSON_GetObjectItemCaseSensitive(a, "path");
    const char *pattern = cJSON_IsString(pv) && *pv->valuestring ? pv->valuestring : NULL;
    const char *glob = cJSON_IsString(gv) && *gv->valuestring ? gv->valuestring : NULL;
    bool icase = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(a, "ignore_case"));
    bool files = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(a, "files_only"));
    if (!pattern && !glob) {
        buf_puts(out, "pattern 和 glob 至少要给一个");
        return false;
    }
    const char *given = cJSON_IsString(pathv) && *pathv->valuestring ? pathv->valuestring : ".";
    char *real = path_resolve(x, given, out);
    if (!real) return false;
    bool ok = false;
    char *rg = NULL, *tool = NULL, *globarg = NULL;
    struct stat st;
    if (!path_can_read(x, real, out)) goto done;
    if (stat(real, &st) < 0) {
        buf_puts(out, "路径不存在：");
        buf_puts(out, real);
        goto done;
    }
    /* 在默认工作目录下执行，结果里的路径尽量是相对路径，短一些 */
    const char *cwd = x->ndirs ? x->dirs[0] : x->home;
    const char *target = real;
    if (path_inside(real, cwd)) target = real[strlen(cwd)] ? real + strlen(cwd) + 1 : ".";

    char *argv[24];
    int n = 0;
    rg = find_in_path(x, "rg");
    if (rg) {
        argv[n++] = rg;
        if (!pattern) {
            argv[n++] = "--files";
        } else {
            argv[n++] = "--no-heading";
            argv[n++] = "--line-number";
            argv[n++] = "--color=never";
            argv[n++] = "--max-columns=500";
            if (icase) argv[n++] = "--ignore-case";
            if (files) argv[n++] = "--files-with-matches";
        }
        if (glob) { argv[n++] = "--glob"; argv[n++] = (char *)glob; }
        if (pattern) { argv[n++] = "--regexp"; argv[n++] = (char *)pattern; }
        argv[n++] = "--";
        argv[n++] = (char *)target;
    } else if (pattern) {
        tool = find_in_path(x, "grep");
        if (!tool) { buf_puts(out, "这台机器上既没有 rg 也没有 grep"); goto done; }
        argv[n++] = tool;
        argv[n++] = "-rnIE";
        argv[n++] = "--exclude-dir=.git";
        if (icase) argv[n++] = "-i";
        if (files) argv[n++] = "-l";
        if (glob) { globarg = xasprintf("--include=%s", glob); argv[n++] = globarg; }
        argv[n++] = "-e";
        argv[n++] = (char *)pattern;
        argv[n++] = "--";
        argv[n++] = (char *)target;
    } else {
        tool = find_in_path(x, "find");
        if (!tool) { buf_puts(out, "这台机器上既没有 rg 也没有 find"); goto done; }
        argv[n++] = tool;
        argv[n++] = (char *)target;
        argv[n++] = "-type";
        argv[n++] = "f";
        argv[n++] = "-name";
        argv[n++] = (char *)glob;
        argv[n++] = "-not";
        argv[n++] = "-path";
        argv[n++] = "*/.git/*";
    }
    argv[n] = NULL;

    proc_status ps;
    if (run_proc(x, argv, cwd, GREP_TIMEOUT, out, &ps) < 0) goto done;
    int code = WIFEXITED(ps.status) ? WEXITSTATUS(ps.status) : -1;
    if (ps.timed_out) {
        put_status(out, &ps, GREP_TIMEOUT);
    } else if (code == 1 && !out->len) {
        buf_puts(out, "没有找到匹配");
        ok = true;
    } else if (code == 0) {
        if (!out->len) buf_puts(out, "没有找到匹配");
        ok = true;
    } else {
        put_status(out, &ps, GREP_TIMEOUT);
    }
done:
    free(real);
    free(rg);
    free(tool);
    free(globarg);
    return ok;
}
