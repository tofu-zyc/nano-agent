#include "tools.h"

#include <ctype.h>
#include <dirent.h>
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

#define STDERR_CAP     8192
#define EXIT_GRACE_MS  2000   /* 进程退出后最多再等这么久让管道关闭 */

/* ---- 加载 ---- */

static char *read_file(const char *path, size_t max, size_t *len)
{
    FILE *f = fopen(path, "rb");
    if (!f) return NULL;
    buf_t b = {0};
    char tmp[4096];
    size_t n;
    while ((n = fread(tmp, 1, sizeof tmp, f)) > 0) {
        buf_append(&b, tmp, n);
        if (b.len > max) break;
    }
    fclose(f);
    if (b.len > max) { buf_free(&b); return NULL; }
    *len = b.len;
    return buf_cstr(&b);
}

static bool valid_name(const char *s)
{
    size_t n = strlen(s);
    if (n == 0 || n > 64) return false;
    for (; *s; s++)
        if (!isalnum((unsigned char)*s) && *s != '_' && *s != '-') return false;
    return true;
}

static void tool_clear(tool_t *t)
{
    free(t->name);
    free(t->dir);
    if (t->argv) {
        for (char **a = t->argv; *a; a++) free(*a);
        free(t->argv);
    }
    cJSON_Delete(t->desc);
    memset(t, 0, sizeof *t);
}

static int load_one(const char *dir, tool_t *t)
{
    memset(t, 0, sizeof *t);
    char *path = xasprintf("%s/tool.json", dir);
    size_t len = 0;
    char *text = read_file(path, 1 << 20, &len);
    if (!text) {
        log_warn("跳过 %s：读不到 tool.json", dir);
        free(path);
        return -1;
    }
    cJSON *j = cJSON_ParseWithLength(text, len);
    free(text);
    if (!cJSON_IsObject(j)) {
        log_warn("跳过 %s：不是合法的 JSON 对象", path);
        goto fail;
    }

    const cJSON *name = cJSON_GetObjectItemCaseSensitive(j, "name");
    if (!cJSON_IsString(name) || !valid_name(name->valuestring)) {
        log_warn("跳过 %s：name 必须是 1~64 位字母、数字、_ 或 -", path);
        goto fail;
    }
    t->name = xstrdup(name->valuestring);
    t->dir = xstrdup(dir);
    t->pol.name = t->name;
    t->pol.timeout_ms = 30 * 1000;
    t->pol.allow_all = false;
    t->pol.approval = false;

    const cJSON *v = cJSON_GetObjectItemCaseSensitive(j, "timeout");
    if (v) {
        if (!cJSON_IsNumber(v) || v->valuedouble < 1 || v->valuedouble > 3600) {
            log_warn("跳过 %s：timeout 应为 1~3600 秒", path);
            goto fail;
        }
        t->pol.timeout_ms = (int)(v->valuedouble * 1000);
    }
    v = cJSON_GetObjectItemCaseSensitive(j, "allow");
    if (v) {
        if (cJSON_IsString(v) && !strcmp(v->valuestring, "all")) t->pol.allow_all = true;
        else if (!(cJSON_IsString(v) && !strcmp(v->valuestring, "admin"))) {
            log_warn("跳过 %s：allow 只能是 \"admin\" 或 \"all\"", path);
            goto fail;
        }
    }
    v = cJSON_GetObjectItemCaseSensitive(j, "approval");
    if (v) {
        if (!cJSON_IsBool(v)) {
            log_warn("跳过 %s：approval 应为 true / false", path);
            goto fail;
        }
        t->pol.approval = cJSON_IsTrue(v);
    }

    v = cJSON_GetObjectItemCaseSensitive(j, "command");
    if (v) {
        int n = cJSON_GetArraySize(v);
        if (!cJSON_IsArray(v) || n < 1) {
            log_warn("跳过 %s：command 应为非空字符串数组", path);
            goto fail;
        }
        t->argv = xcalloc((size_t)n + 1, sizeof *t->argv);
        for (int i = 0; i < n; i++) {
            const cJSON *a = cJSON_GetArrayItem(v, i);
            if (!cJSON_IsString(a)) {
                log_warn("跳过 %s：command 应为字符串数组", path);
                goto fail;
            }
            t->argv[i] = xstrdup(a->valuestring);
        }
    } else {
        t->argv = xcalloc(2, sizeof *t->argv);
        t->argv[0] = xstrdup("./run");
    }
    if (strchr(t->argv[0], '/')) {
        char *exe = t->argv[0][0] == '/' ? xstrdup(t->argv[0]) : xasprintf("%s/%s", dir, t->argv[0]);
        int ok = access(exe, X_OK) == 0;
        free(exe);
        if (!ok) {
            log_warn("跳过 %s：%s 不存在或没有执行权限", dir, t->argv[0]);
            goto fail;
        }
    }

    t->desc = cJSON_CreateObject();
    cJSON_AddStringToObject(t->desc, "name", t->name);
    v = cJSON_GetObjectItemCaseSensitive(j, "description");
    cJSON_AddStringToObject(t->desc, "description", cJSON_IsString(v) ? v->valuestring : "");
    v = cJSON_GetObjectItemCaseSensitive(j, "input_schema");
    if (cJSON_IsObject(v)) {
        cJSON_AddItemToObject(t->desc, "input_schema", cJSON_Duplicate(v, 1));
    } else {
        cJSON *schema = cJSON_AddObjectToObject(t->desc, "input_schema");
        cJSON_AddStringToObject(schema, "type", "object");
        cJSON_AddObjectToObject(schema, "properties");
    }

    cJSON_Delete(j);
    free(path);
    return 0;

fail:
    cJSON_Delete(j);
    free(path);
    tool_clear(t);
    return -1;
}

static int cmp_tool(const void *a, const void *b)
{
    return strcmp(((const tool_t *)a)->name, ((const tool_t *)b)->name);
}

int toolset_load(toolset_t *ts, const char *dir)
{
    memset(ts, 0, sizeof *ts);
    DIR *d = opendir(dir);
    if (!d) return -1;
    struct dirent *e;
    while ((e = readdir(d))) {
        if (e->d_name[0] == '.') continue;
        char *sub = xasprintf("%s/%s", dir, e->d_name);
        struct stat st;
        if (stat(sub, &st) == 0 && S_ISDIR(st.st_mode)) {
            tool_t t;
            if (load_one(sub, &t) == 0) {
                if (toolset_find(ts, t.name)) {
                    log_warn("跳过 %s：工具名 %s 重复", sub, t.name);
                    tool_clear(&t);
                } else {
                    ts->v = xrealloc(ts->v, (ts->n + 1) * sizeof *ts->v);
                    ts->v[ts->n++] = t;
                }
            }
        }
        free(sub);
    }
    closedir(d);
    /* 按名字排序：工具列表稳定，模型那边的提示词前缀缓存才能命中 */
    if (ts->n > 1) qsort(ts->v, ts->n, sizeof *ts->v, cmp_tool);
    for (size_t i = 0; i < ts->n; i++) ts->v[i].pol.name = ts->v[i].name;
    return (int)ts->n;
}

void toolset_free(toolset_t *ts)
{
    for (size_t i = 0; i < ts->n; i++) tool_clear(&ts->v[i]);
    free(ts->v);
    memset(ts, 0, sizeof *ts);
}

const tool_t *toolset_find(const toolset_t *ts, const char *name)
{
    for (size_t i = 0; i < ts->n; i++)
        if (!strcmp(ts->v[i].name, name)) return &ts->v[i];
    return NULL;
}

/* ---- 执行 ---- */

static void close_fd(int *fd)
{
    if (*fd >= 0) close(*fd);
    *fd = -1;
}

int runner_exec(runner_t *r, const tool_t *t, const na_cmd *cmd, char *err, size_t errlen)
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
        /* 子进程：独立进程组，超时时可以连同孙进程一起杀掉 */
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
        if (chdir(t->dir) != 0) {
            dprintf(2, "进入目录 %s 失败：%s\n", t->dir, strerror(errno));
            _exit(127);
        }
        setenv("NA_SID", cmd->sid ? cmd->sid : "", 1);
        setenv("NA_CALL_ID", cmd->id ? cmd->id : "", 1);
        setenv("NA_TOOL", t->name, 1);
        setenv("NA_FROM", cmd->from ? cmd->from : "", 1);
        execvp(t->argv[0], t->argv);
        dprintf(2, "无法执行 %s：%s\n", t->argv[0], strerror(errno));
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
    j->sid = xstrdup(cmd->sid);
    j->id = xstrdup(cmd->id);
    j->name = xstrdup(t->name);
    j->pid = pid;
    j->fd_in = in[1];
    j->fd_out = out[0];
    j->fd_err = er[0];
    j->in = xstrdup(cmd->args && *cmd->args ? cmd->args : "{}");
    j->in_len = strlen(j->in);
    j->next = r->jobs;
    r->jobs = j;
    return 0;

fail:
    close_fd(&in[0]); close_fd(&in[1]);
    close_fd(&out[0]); close_fd(&out[1]);
    close_fd(&er[0]); close_fd(&er[1]);
    return -1;
}

static void kill_job(job_t *j)
{
    if (!j->exited) {
        kill(-j->pid, SIGKILL);
        kill(j->pid, SIGKILL);
    }
    j->killed = true;
}

void runner_kill(runner_t *r, const char *sid, const char *id)
{
    for (job_t *j = r->jobs; j; j = j->next)
        if (!j->killed && !strcmp(j->sid, sid) && !strcmp(j->id, id)) kill_job(j);
}

void runner_kill_all(runner_t *r)
{
    for (job_t *j = r->jobs; j; j = j->next) kill_job(j);
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
        int fds[3] = { j->fd_in, j->fd_out, j->fd_err };
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
            if (j->fd_in >= 0 && j->in_off >= j->in_len) close_fd(&j->fd_in);
            break;
        case 1:
            if (j->fd_out >= 0) drain(&j->fd_out, &j->out, &j->out_total, r->max_output);
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
        /* 进程已退出但管道还开着，说明有后台孙进程占着它；等一会儿就不再等了 */
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

static void append_cut(buf_t *b, const buf_t *src, size_t total, size_t cap)
{
    buf_put_utf8(b, src->p, utf8_cut(src->p, src->len, cap));
    if (total > src->len) {
        char note[96];
        snprintf(note, sizeof note, "\n…（输出过长，已截断；共 %zu 字节）", total);
        buf_puts(b, note);
    }
}

char *job_result(const job_t *j, size_t max_output, bool *ok)
{
    buf_t b = {0};
    bool success = WIFEXITED(j->status) && WEXITSTATUS(j->status) == 0;
    *ok = success;

    if (j->out.len) append_cut(&b, &j->out, j->out_total, max_output);
    if (j->err.len && (!success || b.len == 0)) {
        if (b.len) buf_puts(&b, "\n[stderr]\n");
        append_cut(&b, &j->err, j->err_total, STDERR_CAP);
    }
    if (!success) {
        char note[64];
        if (WIFEXITED(j->status)) snprintf(note, sizeof note, "（退出码 %d）", WEXITSTATUS(j->status));
        else if (WIFSIGNALED(j->status)) snprintf(note, sizeof note, "（被信号 %d 终止）", WTERMSIG(j->status));
        else snprintf(note, sizeof note, "（异常结束）");
        if (b.len) buf_puts(&b, "\n");
        buf_puts(&b, note);
    }
    if (b.len == 0) buf_puts(&b, "（无输出）");
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
