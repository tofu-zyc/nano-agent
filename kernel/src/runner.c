/* runner.c —— 子进程任务，见 runner.h */
#include "runner.h"

#include "cJSON.h"

#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#define STDERR_CAP     8192
#define EXIT_GRACE_MS  2000   /* 进程退出后最多再等这么久让管道关闭 */
#define KILL_GRACE_MS  2000   /* SIGTERM 之后最多等这么久，再 SIGKILL */

static void close_fd(int *fd)
{
    if (*fd >= 0) close(*fd);
    *fd = -1;
}

int runner_start(runner_t *r, char *const argv[], const char *sid, const char *id, const char *name,
                 char *input, char *err, size_t errlen)
{
    int in[2] = { -1, -1 }, out[2] = { -1, -1 }, er[2] = { -1, -1 };
    if (pipe(in) || pipe(out) || pipe(er)) {
        snprintf(err, errlen, "创建管道失败：%s", strerror(errno));
        goto fail;
    }
    int fds[] = { in[0], in[1], out[0], out[1], er[0], er[1] };
    for (size_t i = 0; i < 6; i++) set_cloexec(fds[i]);

    pid_t pid = fork();
    if (pid < 0) {
        snprintf(err, errlen, "fork 失败：%s", strerror(errno));
        goto fail;
    }
    if (pid == 0) {
        /* 子进程：独立进程组，终止时可以连同它启动的进程一起处理 */
        setpgid(0, 0);
        signal(SIGPIPE, SIG_DFL);
        signal(SIGCHLD, SIG_DFL);
        signal(SIGINT, SIG_DFL);
        signal(SIGTERM, SIG_DFL);
        sigset_t none;
        sigemptyset(&none);
        sigprocmask(SIG_SETMASK, &none, NULL);
        dup2(in[0], 0);
        dup2(out[1], 1);
        dup2(er[1], 2);
        execvp(argv[0], argv);
        dprintf(2, "无法执行 %s：%s\n", argv[0], strerror(errno));
        _exit(127);
    }

    setpgid(pid, pid);   /* 和子进程里的调用重复，消除竞态；失败无妨 */
    close(in[0]);
    close(out[1]);
    close(er[1]);
    set_nonblock(in[1]);
    set_nonblock(out[0]);
    set_nonblock(er[0]);

    job_t *j = xcalloc(1, sizeof *j);
    j->sid = xstrdup(sid);
    j->id = xstrdup(id);
    j->name = xstrdup(name);
    j->pid = pid;
    j->fd_in = in[1];
    j->fd_out = out[0];
    j->fd_err = er[0];
    j->in = input;
    j->in_len = strlen(input);
    j->next = r->jobs;
    r->jobs = j;
    return 0;

fail:
    free(input);
    close_fd(&in[0]); close_fd(&in[1]);
    close_fd(&out[0]); close_fd(&out[1]);
    close_fd(&er[0]); close_fd(&er[1]);
    return -1;
}

static void stop_job(job_t *j)
{
    if (j->killed) return;
    j->killed = true;
    close_fd(&j->fd_in);   /* 远程执行时，这是唯一能传过去的取消信号 */
    if (!j->exited) {
        kill(-j->pid, SIGTERM);
        j->term_at = now_ms();
    }
}

void runner_kill(runner_t *r, const char *sid, const char *id)
{
    for (job_t *j = r->jobs; j; j = j->next)
        if (!strcmp(j->sid, sid) && !strcmp(j->id, id)) stop_job(j);
}

void runner_kill_all(runner_t *r)
{
    for (job_t *j = r->jobs; j; j = j->next) stop_job(j);
}

void runner_tick(runner_t *r, int64_t now)
{
    for (job_t *j = r->jobs; j; j = j->next) {
        if (j->killed && !j->exited && j->term_at && now - j->term_at >= KILL_GRACE_MS) {
            kill(-j->pid, SIGKILL);
            kill(j->pid, SIGKILL);
            j->term_at = 0;
        }
    }
}

void runner_reap(runner_t *r, int64_t now)
{
    int st;
    pid_t pid;
    while ((pid = waitpid(-1, &st, WNOHANG)) > 0) {
        for (job_t *j = r->jobs; j; j = j->next) {
            if (j->pid == pid && !j->exited) {
                j->exited = true;
                j->status = st;
                j->exit_at = now;
                break;
            }
        }
    }
}

size_t runner_pollfds(runner_t *r, struct pollfd *pfds, pollref_t *refs, size_t max)
{
    size_t n = 0;
    for (job_t *j = r->jobs; j; j = j->next) {
        /* stdin 写完后保持打开但不再关心，关闭它表示取消 */
        int fds[3] = { j->in_off < j->in_len ? j->fd_in : -1, j->fd_out, j->fd_err };
        for (int w = 0; w < 3 && n < max; w++) {
            if (fds[w] < 0) continue;
            pfds[n].fd = fds[w];
            pfds[n].events = w == 0 ? POLLOUT : POLLIN;
            pfds[n].revents = 0;
            refs[n].job = j;
            refs[n].which = w;
            n++;
        }
    }
    return n;
}

/* 读空管道（每次最多读一定量，避免被疯狂输出的进程卡住） */
static void drain(int *fd, buf_t *b, size_t *total, size_t cap)
{
    char tmp[16384];
    for (int i = 0; i < 16; i++) {
        ssize_t n = read(*fd, tmp, sizeof tmp);
        if (n > 0) {
            *total += (size_t)n;
            if (b->len < cap) {
                size_t room = cap - b->len;
                buf_append(b, tmp, (size_t)n < room ? (size_t)n : room);
            }
            continue;
        }
        if (n < 0 && errno == EINTR) continue;
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) return;
        close_fd(fd);   /* EOF 或出错 */
        return;
    }
}

void runner_on_poll(runner_t *r, const struct pollfd *pfds, const pollref_t *refs, size_t n)
{
    for (size_t i = 0; i < n; i++) {
        job_t *j = refs[i].job;
        short re = pfds[i].revents;
        if (!re) continue;
        switch (refs[i].which) {
        case 0:
            if (j->fd_in < 0) break;
            if (re & (POLLERR | POLLHUP | POLLNVAL)) {
                close_fd(&j->fd_in);
                break;
            }
            {
                ssize_t w = write(j->fd_in, j->in + j->in_off, j->in_len - j->in_off);
                if (w > 0) j->in_off += (size_t)w;
                else if (w < 0 && errno != EAGAIN && errno != EINTR) close_fd(&j->fd_in);
            }
            break;
        case 1:
            if (j->fd_out >= 0) drain(&j->fd_out, &j->out, &j->out_total, r->out_cap);
            break;
        case 2:
            if (j->fd_err >= 0) drain(&j->fd_err, &j->err, &j->err_total, STDERR_CAP);
            break;
        }
    }
}

job_t *runner_take_finished(runner_t *r, int64_t now)
{
    for (job_t **pp = &r->jobs; *pp; pp = &(*pp)->next) {
        job_t *j = *pp;
        if (!j->exited) continue;
        bool drained = j->fd_out < 0 && j->fd_err < 0;
        /* 进程已退出但管道还开着，说明有后台进程占着它；等一会儿就不再等了 */
        if (!drained && now - j->exit_at < EXIT_GRACE_MS) continue;
        close_fd(&j->fd_in);
        close_fd(&j->fd_out);
        close_fd(&j->fd_err);
        *pp = j->next;
        j->next = NULL;
        return j;
    }
    return NULL;
}

size_t runner_count(const runner_t *r)
{
    size_t n = 0;
    for (const job_t *j = r->jobs; j; j = j->next) n++;
    return n;
}

char *job_result(const job_t *j, bool *ok)
{
    *ok = false;
    cJSON *res = j->out_total <= j->out.len ? cJSON_ParseWithLength(j->out.p ? j->out.p : "", j->out.len) : NULL;
    const cJSON *okv = cJSON_GetObjectItemCaseSensitive(res, "ok");
    const cJSON *content = cJSON_GetObjectItemCaseSensitive(res, "content");
    if (cJSON_IsBool(okv) && cJSON_IsString(content)) {
        *ok = cJSON_IsTrue(okv);
        buf_t b = {0};
        buf_put_utf8(&b, content->valuestring, strlen(content->valuestring));
        cJSON_Delete(res);
        return buf_cstr(&b);
    }
    cJSON_Delete(res);

    /* 执行器没有正常给出结果：多半是崩溃了、被杀了，或者根本没启动起来 */
    buf_t b = {0};
    char note[128];
    if (WIFEXITED(j->status)) snprintf(note, sizeof note, "执行器异常结束（退出码 %d）", WEXITSTATUS(j->status));
    else if (WIFSIGNALED(j->status)) snprintf(note, sizeof note, "执行器被信号 %d 终止", WTERMSIG(j->status));
    else snprintf(note, sizeof note, "执行器异常结束");
    buf_puts(&b, note);
    if (j->out_total > j->out.len) buf_puts(&b, "：输出超出上限");
    if (j->err.len) {
        buf_puts(&b, "：");
        buf_put_utf8(&b, j->err.p, utf8_cut(j->err.p, j->err.len, STDERR_CAP));
    }
    return buf_cstr(&b);
}

void job_free(job_t *j)
{
    if (!j) return;
    close_fd(&j->fd_in);
    close_fd(&j->fd_out);
    close_fd(&j->fd_err);
    free(j->sid);
    free(j->id);
    free(j->name);
    free(j->in);
    buf_free(&j->out);
    buf_free(&j->err);
    free(j);
}
