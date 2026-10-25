/* pipe.c —— 管道透传的实现（POSIX 用 poll，Windows 暂时不支持） */
#include "pipe.h"

#include "util.h"

#include <errno.h>
#include <stdio.h>
#include <string.h>

#if !NA_WINDOWS

#include <fcntl.h>
#include <signal.h>
#include <stdlib.h>
#include <sys/wait.h>
#include <unistd.h>

#define READ_CHUNK 65536

void pipes_init(na_pipes *ps)
{
    memset(ps, 0, sizeof *ps);
    ps->close_grace_ms = 3000;
}

static na_pipe_t *find(na_pipes *ps, const char *id)
{
    for (na_pipe_t *p = ps->list; p; p = p->next)
        if (!strcmp(p->id, id)) return p;
    return NULL;
}

/* 往队列里放一条（qhead 前面已经发掉的格子复用） */
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

static void queue_line(na_pipes *ps, na_pipe_t *p, const char *line, size_t n)
{
    na_pipe_msg *m = slot(ps);
    m->id = xstrdup(p->id);
    char *s = xcalloc(1, n + 1);
    memcpy(s, line, n);
    m->data = s;
}

static void queue_closed(na_pipes *ps, na_pipe_t *p, int code, const char *detail)
{
    na_pipe_msg *m = slot(ps);
    m->id = xstrdup(p->id);
    m->exit = code;
    m->detail = xstrdup(detail ? detail : "");
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
    pid_t pid = fork();
    if (pid < 0) {
        snprintf(err, errlen, "fork 失败：%s", strerror(errno));
        close(in[0]); close(in[1]); close(out[0]); close(out[1]);
        return -1;
    }
    if (pid == 0) {
        /* 子进程：接好三个标准流，设置环境变量，切目录，然后 exec */
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
    close(in[0]);
    close(out[1]);
    fcntl(in[1], F_SETFL, fcntl(in[1], F_GETFL) | O_NONBLOCK);
    fcntl(out[0], F_SETFL, fcntl(out[0], F_GETFL) | O_NONBLOCK);
    na_pipe_t *p = xcalloc(1, sizeof *p);
    p->id = xstrdup(id);
    p->fd_in = in[1];
    p->fd_out = out[0];
    p->pid = (int)pid;
    p->next = ps->list;
    ps->list = p;
    return 0;
}

int pipes_write(na_pipes *ps, const char *id, const char *data)
{
    na_pipe_t *p = find(ps, id);
    if (!p || p->fd_in < 0) return -1;
    size_t n = strlen(data ? data : "");
    char *line = xcalloc(1, n + 2);
    memcpy(line, data ? data : "", n);
    line[n] = '\n';
    size_t off = 0, total = n + 1;
    int rc = 0;
    while (off < total) {
        ssize_t w = write(p->fd_in, line + off, total - off);
        if (w > 0) { off += (size_t)w; continue; }
        if (w < 0 && (errno == EAGAIN || errno == EINTR)) continue;
        rc = -1;
        break;
    }
    free(line);
    return rc;
}

void pipes_close(na_pipes *ps, const char *id)
{
    na_pipe_t *p = find(ps, id);
    if (!p) return;
    if (p->fd_in >= 0) {
        close(p->fd_in);
        p->fd_in = -1;
    }
    p->closing = true;
    p->close_at = now_ms();
}

size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_pipe_t **refs, size_t max)
{
    size_t n = 0;
    for (na_pipe_t *p = ps->list; p && n < max; p = p->next) {
        if (p->fd_out < 0) continue;
        pfds[n] = (na_pollfd){ .id = p->fd_out, .events = NA_POLLIN };
        refs[n] = p;
        n++;
    }
    return n;
}

/* 从一条管道里读干净，切出完整的行 */
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
                queue_line(ps, p, p->buf + start, len);
                start = i + 1;
            }
            if (start) {
                memmove(p->buf, p->buf + start, p->len - start);
                p->len -= start;
            }
            continue;
        }
        if (n == 0 || (errno != EAGAIN && errno != EINTR)) {
            if (p->fd_out >= 0) {
                close(p->fd_out);
                p->fd_out = -1;
            }
        }
        return;
    }
}

void pipes_poll(na_pipes *ps, const na_pollfd *pfds, na_pipe_t **refs, size_t n, int64_t now)
{
    for (size_t i = 0; i < n; i++) {
        if (!pfds[i].revents) continue;
        if (refs[i] && refs[i]->fd_out >= 0) drain_pipe(ps, refs[i]);
    }
    /* 收尸 + 关了 stdin 迟迟不退的，先 SIGTERM 再 SIGKILL */
    for (na_pipe_t *p = ps->list; p; p = p->next) {
        if (p->reported) continue;
        if (p->pid > 0) {
            int status = 0;
            pid_t got = waitpid((pid_t)p->pid, &status, WNOHANG);
            if (got < 0 && errno == ECHILD) {
                /* 已经被别处收走了（执行器用的是 waitpid(-1)）：当作已退出 */
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
            queue_closed(ps, p, p->code, "");
            p->reported = true;
            if (p->fd_in >= 0) { close(p->fd_in); p->fd_in = -1; }
            if (p->fd_out >= 0) { close(p->fd_out); p->fd_out = -1; }
            continue;
        }
        if (p->closing && p->pid > 0) {
            int64_t waited = now - p->close_at;
            if (waited > ps->close_grace_ms) {
                kill((pid_t)p->pid, SIGKILL);
            } else if (waited > ps->close_grace_ms / 2) {
                kill((pid_t)p->pid, SIGTERM);
            }
        }
    }
}

bool pipes_take(na_pipes *ps, na_pipe_msg *out)
{
    if (ps->qhead >= ps->qn) {
        ps->qhead = ps->qn = 0;
        return false;
    }
    *out = ps->q[ps->qhead];
    ps->q[ps->qhead].id = ps->q[ps->qhead].data = ps->q[ps->qhead].detail = NULL;
    ps->qhead++;
    return true;
}

void pipes_shutdown(na_pipes *ps)
{
    for (na_pipe_t *p = ps->list; p; p = p->next) {
        if (p->fd_in >= 0) { close(p->fd_in); p->fd_in = -1; }
        if (p->fd_out >= 0) { close(p->fd_out); p->fd_out = -1; }
        if (p->pid > 0 && !p->reaped) {
            kill((pid_t)p->pid, SIGTERM);
            kill((pid_t)p->pid, SIGKILL);
            waitpid((pid_t)p->pid, NULL, 0);
            p->reaped = true;
            p->pid = -1;
        }
    }
}

#else /* NA_WINDOWS */

void pipes_init(na_pipes *ps) { memset(ps, 0, sizeof *ps); }

int pipes_open(na_pipes *ps, const char *id, char *const argv[], const char *const env[], const char *cwd,
               char *err, size_t errlen)
  { (void)ps; (void)id; (void)argv; (void)env; (void)cwd; snprintf(err, errlen, "这台机器（Windows）的内核还不支持 MCP 透传"); return -1; }
int pipes_write(na_pipes *ps, const char *id, const char *data) { (void)ps; (void)id; (void)data; return -1; }
void pipes_close(na_pipes *ps, const char *id) { (void)ps; (void)id; }
size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_pipe_t **refs, size_t max) { (void)ps; (void)pfds; (void)refs; (void)max; return 0; }
void pipes_poll(na_pipes *ps, const na_pollfd *pfds, na_pipe_t **refs, size_t n, int64_t now) { (void)ps; (void)pfds; (void)refs; (void)n; (void)now; }
bool pipes_take(na_pipes *ps, na_pipe_msg *out) { (void)ps; (void)out; return false; }
void pipes_shutdown(na_pipes *ps) { (void)ps; }

#endif
