/* pipe.c —— 管道透传：共用部分 + POSIX 实现（Windows 见 pipe_win.c） */
#include "pipe.h"

#include "util.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* ---- 两边共用 ---- */

na_pipe_t *find_pipe(na_pipes *ps, const char *id)
{
    if (!id) return NULL;
    for (na_pipe_t *p = ps->list; p; p = p->next)
        if (!strcmp(p->id, id)) return p;
    return NULL;
}

size_t pipes_count(const na_pipes *ps)
{
    size_t n = 0;
    for (const na_pipe_t *p = ps->list; p; p = p->next) n++;
    return n;
}

void free_pipe(na_pipe_t *p)
{
    if (!p) return;
    free(p->id);
    free(p->buf);
    free(p->wbuf);
    free(p);
}

/* 丢掉还没发出去的消息（网关断开了，发给下一个网关没有意义） */
void pipes_clear_queue(na_pipes *ps)
{
    pipes_lock(ps);
    for (size_t i = ps->qhead; i < ps->qn; i++) {
        free(ps->q[i].id);
        free(ps->q[i].data);
        free(ps->q[i].detail);
    }
    ps->qn = ps->qhead = 0;
    pipes_unlock(ps);
}

#if !NA_WINDOWS
void pipes_lock(na_pipes *ps) { (void)ps; }
void pipes_unlock(na_pipes *ps) { (void)ps; }
void pipes_set_wake(na_pipes *ps, na_wake *w) { (void)ps; (void)w; }
#endif

static na_pipe_msg *slot(na_pipes *ps)
{
    if (ps->qhead > 0 && ps->qn == ps->qcap) {
        size_t keep = ps->qn - ps->qhead;
        memmove(ps->q, ps->q + ps->qhead, keep * sizeof *ps->q);
        ps->qn = keep;
        ps->qhead = 0;
    }
    if (ps->qn == ps->qcap) {
        size_t cap = ps->qcap ? ps->qcap * 2 : 16;
        na_pipe_msg *q = xrealloc(ps->q, cap * sizeof *q);
        memset(q + ps->qcap, 0, (cap - ps->qcap) * sizeof *q);
        ps->q = q;
        ps->qcap = cap;
    }
    na_pipe_msg *m = &ps->q[ps->qn++];
    m->id = m->data = m->detail = NULL;
    m->exit = -1;
    return m;
}

void pipes_queue_line(na_pipes *ps, const char *id, const char *line, size_t n)
{
    /* 不合法的 UTF-8（比如中文 Windows 上程序输出的 GBK）换成 U+FFFD：发出去的 JSON 必须合法 */
    buf_t b = {0};
    buf_put_utf8(&b, line, n);
    buf_append(&b, "", 1);
    pipes_lock(ps);
    na_pipe_msg *m = slot(ps);
    m->id = xstrdup(id ? id : "");
    m->data = b.p;
    pipes_unlock(ps);
}

void pipes_queue_closed(na_pipes *ps, const char *id, int code, const char *detail)
{
    pipes_lock(ps);
    na_pipe_msg *m = slot(ps);
    m->id = xstrdup(id ? id : "");
    m->exit = code;
    m->detail = xstrdup(detail ? detail : "");
    pipes_unlock(ps);
}

bool pipes_take(na_pipes *ps, na_pipe_msg *out)
{
    pipes_lock(ps);
    if (ps->qhead >= ps->qn) {
        ps->qhead = ps->qn = 0;
        pipes_unlock(ps);
        return false;
    }
    *out = ps->q[ps->qhead];
    ps->q[ps->qhead].id = ps->q[ps->qhead].data = ps->q[ps->qhead].detail = NULL;
    ps->qhead++;
    pipes_unlock(ps);
    return true;
}

#if !NA_WINDOWS

#include <fcntl.h>
#include <signal.h>
#include <stdlib.h>
#include <sys/wait.h>
#include <unistd.h>

#define READ_CHUNK 65536
#define WRITE_CAP  (8u << 20)   /* 往 stdin 最多攒这么多，再多就是服务不读了 */

void pipes_init(na_pipes *ps)
{
    memset(ps, 0, sizeof *ps);
    ps->close_grace_ms = 3000;
}

/* 关掉 stdin（对端会读到 EOF） */
static void close_in(na_pipe_t *p)
{
    if (p->fd_in >= 0) {
        close(p->fd_in);
        p->fd_in = -1;
    }
    p->input_dead = true;
    p->wlen = p->woff = 0;
}

static void close_out(na_pipe_t *p)
{
    if (p->fd_out >= 0) {
        close(p->fd_out);
        p->fd_out = -1;
    }
}

static int pipe_nonblock(int fd)
{
    int fl = fcntl(fd, F_GETFL);
    return fl < 0 ? -1 : fcntl(fd, F_SETFL, fl | O_NONBLOCK);
}

int pipes_open(na_pipes *ps, const char *id, char *const argv[], const char *const env[], const char *cwd,
               char *err, size_t errlen)
{
    if (!argv || !argv[0]) {
        snprintf(err, errlen, "没有要跑的命令");
        return -1;
    }
    int in[2], out[2];
    if (pipe(in) < 0 || pipe(out) < 0) {
        snprintf(err, errlen, "建管道失败：%s", strerror(errno));
        return -1;
    }
    /* 一定要 close-on-exec：不然 Shell 命令和别的 MCP 进程都会继承我们的管道 */
    fcntl(in[0], F_SETFD, FD_CLOEXEC); fcntl(in[1], F_SETFD, FD_CLOEXEC);
    fcntl(out[0], F_SETFD, FD_CLOEXEC); fcntl(out[1], F_SETFD, FD_CLOEXEC);
    pid_t pid = fork();
    if (pid < 0) {
        snprintf(err, errlen, "fork 失败：%s", strerror(errno));
        close(in[0]); close(in[1]); close(out[0]); close(out[1]);
        return -1;
    }
    if (pid == 0) {
        /* 独立进程组：结束的时候能把 npx 拉起来的 node 一起杀掉 */
        setpgid(0, 0);
        dup2(in[0], 0);
        dup2(out[1], 1);
        int devnull = open("/dev/null", O_WRONLY);
        if (devnull >= 0) dup2(devnull, 2);
        close(in[0]); close(in[1]); close(out[0]); close(out[1]);
        if (devnull >= 0) close(devnull);
        if (cwd && *cwd && chdir(cwd) != 0) _exit(126);
        for (size_t i = 0; env && env[i] && env[i + 1]; i += 2) setenv(env[i], env[i + 1], 1);
        setenv("NANO_MCP", "1", 1);
        execvp(argv[0], argv);
        _exit(127);
    }
    setpgid(pid, pid);   /* 和子进程抢一下，谁先都行 */
    close(in[0]);
    close(out[1]);
    pipe_nonblock(in[1]);
    pipe_nonblock(out[0]);
    na_pipe_t *p = xcalloc(1, sizeof *p);
    p->id = xstrdup(id);
    p->fd_in = in[1];
    p->fd_out = out[0];
    p->pid = (int)pid;
    p->next = ps->list;
    ps->list = p;
    return 0;
}

/* 把攒着的写进 stdin，写不动就留着，下一次可写时再来 */
static void flush_in(na_pipe_t *p)
{
    while (!p->input_dead && p->woff < p->wlen) {
        ssize_t w = write(p->fd_in, p->wbuf + p->woff, p->wlen - p->woff);
        if (w > 0) {
            p->woff += (size_t)w;
            continue;
        }
        if (w < 0 && (errno == EAGAIN || errno == EINTR)) return;
        close_in(p);   /* 对面不读了 */
        return;
    }
    if (p->woff >= p->wlen) p->wlen = p->woff = 0;
}

int pipes_write(na_pipes *ps, const char *id, const char *data)
{
    na_pipe_t *p = find_pipe(ps, id);
    if (!p || p->fd_in < 0) {
        /* 网关还当这条管道活着（内核重启过，或者进程的 stdin 已经坏了）：回一条退出，别让它一直等 */
        pipes_queue_closed(ps, id ? id : "", -1, p ? "进程不再读 stdin 了" : "没有这条管道");
        if (p && !p->closing) {   /* 进程可能还活着：按关闭处理，到点不退就杀 */
            p->closing = true;
            p->close_at = now_ms();
        }
        return -1;
    }
    size_t n = strlen(data ? data : "");
    if (p->wlen - p->woff + n + 1 > WRITE_CAP) {
        pipes_queue_closed(ps, id, -1, "写给 stdin 的数据太多，服务好像不读了");
        close_in(p);
        p->closing = true;   /* 网关那边已经当它结束了，不会再发 pipe_close：自己收掉 */
        p->close_at = now_ms();
        return -1;
    }
    p->wbuf = xrealloc(p->wbuf, p->wlen + n + 1);
    if (n) memcpy(p->wbuf + p->wlen, data, n);
    p->wbuf[p->wlen + n] = '\n';
    p->wlen += n + 1;
    flush_in(p);   /* 能写就当场写掉，写不动的留在缓冲里等 poll */
    return 0;
}

void pipes_close(na_pipes *ps, const char *id)
{
    na_pipe_t *p = find_pipe(ps, id);
    if (!p) {
        pipes_queue_closed(ps, id ? id : "", -1, "没有这条管道");
        return;
    }
    flush_in(p);
    close_in(p);
    p->closing = true;
    p->close_at = now_ms();
}

size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_piperef *refs, size_t max)
{
    size_t n = 0;
    for (na_pipe_t *p = ps->list; p && n < max; p = p->next) {
        if (p->fd_in >= 0 && p->wlen > p->woff) {
            pfds[n] = (na_pollfd){ .id = p->fd_in, .events = NA_POLLOUT };
            refs[n].p = p;
            refs[n].which = 0;
            n++;
            if (n >= max) break;
        }
        if (p->fd_out >= 0) {
            pfds[n] = (na_pollfd){ .id = p->fd_out, .events = NA_POLLIN };
            refs[n].p = p;
            refs[n].which = 1;
            n++;
        }
    }
    return n;
}

static void drain_pipe(na_pipes *ps, na_pipe_t *p)
{
    char tmp[READ_CHUNK];
    for (;;) {
        ssize_t n = read(p->fd_out, tmp, sizeof tmp);
        if (n > 0) {
            if (p->len + (size_t)n + 1 > p->cap) {
                size_t cap = p->cap ? p->cap : 4096;
                while (cap < p->len + (size_t)n + 1) cap *= 2;
                p->buf = xrealloc(p->buf, cap);
                p->cap = cap;
            }
            memcpy(p->buf + p->len, tmp, (size_t)n);
            p->len += (size_t)n;
            size_t start = 0;
            for (size_t i = 0; i < p->len; i++) {
                if (p->buf[i] != '\n') continue;
                size_t len = i - start;
                if (len > 0 && p->buf[start + len - 1] == '\r') len--;
                pipes_queue_line(ps, p->id, p->buf + start, len);
                start = i + 1;
            }
            if (start) {
                memmove(p->buf, p->buf + start, p->len - start);
                p->len -= start;
            }
            continue;
        }
        if (n == 0 || (errno != EAGAIN && errno != EINTR)) close_out(p);
        return;
    }
}

void pipes_poll(na_pipes *ps, const na_pollfd *pfds, const na_piperef *refs, size_t n, int64_t now)
{
    for (size_t i = 0; i < n; i++) {
        na_pipe_t *p = refs[i].p;
        if (!p) continue;
        short re = pfds[i].revents;
        if (refs[i].which == 0) {
            if (re & (NA_POLLERR | NA_POLLHUP | NA_POLLNVAL)) close_in(p);
            else if (re & NA_POLLOUT) flush_in(p);
        } else if (p->fd_out >= 0 && re) {
            drain_pipe(ps, p);
        }
    }
    for (na_pipe_t *p = ps->list; p; p = p->next) {
        if (p->reported) continue;
        if (p->pid > 0) {
            int status = 0;
            pid_t got = waitpid((pid_t)p->pid, &status, WNOHANG);
            if (got < 0 && errno == ECHILD) {
                p->reaped = true;
                p->pid = -1;
                p->code = 0;
                got = 0;
            }
            if (got == (pid_t)p->pid) {
                p->reaped = true;
                p->pid = -1;
                if (WIFEXITED(status)) p->code = WEXITSTATUS(status);
                else if (WIFSIGNALED(status)) p->code = 128 + WTERMSIG(status);
                else p->code = -1;
            }
        }
        if (p->reaped) {
            if (p->fd_out >= 0) drain_pipe(ps, p);
            pipes_queue_closed(ps, p->id, p->code, "");
            p->reported = true;
            close_in(p);
            close_out(p);
            continue;
        }
        if (p->closing && p->pid > 0) {
            int64_t waited = now - p->close_at;
            /* 杀整个进程组：npx 起来了 node，只杀 npx 会留下 node */
            if (waited > ps->close_grace_ms) kill(-(pid_t)p->pid, SIGKILL);
            else if (waited > ps->close_grace_ms / 2) kill(-(pid_t)p->pid, SIGTERM);
        }
    }
    /* 已经报过退出的条目清掉 */
    na_pipe_t **pp = &ps->list;
    while (*pp) {
        na_pipe_t *p = *pp;
        if (p->reported && p->fd_in < 0 && p->fd_out < 0) {
            *pp = p->next;
            free_pipe(p);
            continue;
        }
        pp = &p->next;
    }
}

/* 杀掉进程组、关掉两头，标成已经报告过（不再发 pipe_closed）。条目本身留着。 */
static void kill_pipe(na_pipe_t *p)
{
    close_in(p);
    close_out(p);
    if (p->pid > 0 && !p->reaped) {
        kill(-(pid_t)p->pid, SIGTERM);
        kill(-(pid_t)p->pid, SIGKILL);
        waitpid((pid_t)p->pid, NULL, 0);
        p->reaped = true;
        p->pid = -1;
    }
    p->reported = true;
}

void pipes_reset(na_pipes *ps)
{
    for (na_pipe_t *p = ps->list; p; p = p->next) kill_pipe(p);
    pipes_clear_queue(ps);
    /* 不在这里 free：同一轮主循环里 pipes_poll 拿着的引用还指着这些条目，
       它们已经 reported、两头都关了，pipes_poll 收尾时会释放 */
}

void pipes_shutdown(na_pipes *ps)
{
    na_pipe_t *p = ps->list;
    while (p) {
        na_pipe_t *next = p->next;
        kill_pipe(p);
        free_pipe(p);
        p = next;
    }
    ps->list = NULL;
    pipes_clear_queue(ps);
}

#endif /* !NA_WINDOWS */
