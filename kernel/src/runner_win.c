/* runner_win.c —— 子进程任务（Windows），见 runner.h
 *
 * 主循环还是 WSAPoll：执行器子进程的输出由两个读线程读，读完通过唤醒 socket
 * 叫醒主循环；stdin 保持打开，关掉它就是取消。超时或取消时 TerminateJobObject
 * 会把执行器及其子进程一起杀掉。
 */
#define WIN32_LEAN_AND_MEAN
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0A00
#endif
#include <winsock2.h>
#include <windows.h>
#include <errno.h>
#include <process.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <wchar.h>

#include "runner.h"
#include "cJSON.h"

#define STDERR_CAP     8192
#define EXIT_GRACE_MS  2000
#define KILL_GRACE_MS  2000

static wchar_t *wdup(const wchar_t *s)
{
    size_t n = wcslen(s);
    wchar_t *p = xmalloc((n + 1) * sizeof *p);
    memcpy(p, s, (n + 1) * sizeof *p);
    return p;
}

static wchar_t *to_w(const char *s)
{
    int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, NULL, 0);
    if (n <= 0) return NULL;
    wchar_t *w = xmalloc((size_t)n * sizeof *w);
    if (!MultiByteToWideChar(CP_UTF8, 0, s, -1, w, n)) { free(w); return NULL; }
    return w;
}

static wchar_t *cmdline_w(char *const argv[])
{
    wchar_t *w = to_w(argv[0]);
    if (!w) return wdup(L"nano-exec.exe");
    if (!wcschr(w, L' ')) return w;
    size_t l = wcslen(w);
    wchar_t *q = xmalloc((l + 3) * sizeof *q);
    q[0] = L'"';
    memcpy(q + 1, w, l * sizeof *q);
    q[l + 1] = L'"';
    q[l + 2] = 0;
    free(w);
    return q;
}

static void close_handle(HANDLE *h)
{
    if (*h && *h != INVALID_HANDLE_VALUE) CloseHandle(*h);
    *h = NULL;
}

typedef struct {
    job_t  *job;
    HANDLE  h;
    int     which;   /* 1 = stdout, 2 = stderr */
    runner_t *r;
} reader_arg;

static DWORD WINAPI reader_thread(LPVOID p);

int runner_start(runner_t *r, char *const argv[], const char *sid, const char *id, const char *name,
                 char *input, char *err, size_t errlen)
{
    return runner_start_ex(r, argv, sid, id, name, input, strlen(input), err, errlen) ? 0 : -1;
}

job_t *runner_start_ex(runner_t *r, char *const argv[], const char *sid, const char *id, const char *name,
                       char *input, size_t in_len, char *err, size_t errlen)
{
    SECURITY_ATTRIBUTES sa = { sizeof sa, NULL, TRUE };
    HANDLE in_r = NULL, in_w = NULL, out_r = NULL, out_w = NULL, er_r = NULL, er_w = NULL;
    if (!CreatePipe(&in_r, &in_w, &sa, 0) || !CreatePipe(&out_r, &out_w, &sa, 0) ||
        !CreatePipe(&er_r, &er_w, &sa, 0)) {
        snprintf(err, errlen, "创建管道失败：错误 %lu", (unsigned long)GetLastError());
        goto fail;
    }
    SetHandleInformation(in_w, HANDLE_FLAG_INHERIT, 0);
    SetHandleInformation(out_r, HANDLE_FLAG_INHERIT, 0);
    SetHandleInformation(er_r, HANDLE_FLAG_INHERIT, 0);

    STARTUPINFOEXW si;
    ZeroMemory(&si, sizeof si);
    si.StartupInfo.cb = sizeof si;
    si.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    si.StartupInfo.hStdInput = in_r;
    si.StartupInfo.hStdOutput = out_w;
    si.StartupInfo.hStdError = er_w;
    SIZE_T attr_size = 0;
    InitializeProcThreadAttributeList(NULL, 1, 0, &attr_size);
    si.lpAttributeList = xmalloc(attr_size ? attr_size : 1);
    bool have_attr = attr_size && InitializeProcThreadAttributeList(si.lpAttributeList, 1, 0, &attr_size);
    if (have_attr) {
        HANDLE keep[3] = { in_r, out_w, er_w };
        UpdateProcThreadAttribute(si.lpAttributeList, 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
                                  keep, sizeof keep, NULL, NULL);
    } else {
        si.StartupInfo.cb = sizeof(STARTUPINFO);
    }
    wchar_t *line = cmdline_w(argv);
    DWORD flags = CREATE_SUSPENDED | CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT;
    if (have_attr) flags |= EXTENDED_STARTUPINFO_PRESENT;
    PROCESS_INFORMATION pi;
    ZeroMemory(&pi, sizeof pi);
    BOOL ok = CreateProcessW(NULL, line, NULL, NULL, TRUE, flags, NULL, NULL, &si.StartupInfo, &pi);
    DWORD e = GetLastError();
    if (have_attr) DeleteProcThreadAttributeList(si.lpAttributeList);
    free(si.lpAttributeList);
    free(line);
    if (!ok) {
        snprintf(err, errlen, "无法启动执行器 %s：错误 %lu", argv[0], (unsigned long)e);
        goto fail;
    }
    CloseHandle(in_r);
    CloseHandle(out_w);
    CloseHandle(er_w);
    in_r = out_w = er_w = NULL;

    HANDLE job = CreateJobObjectW(NULL, NULL);
    if (job) {
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION li;
        ZeroMemory(&li, sizeof li);
        li.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        SetInformationJobObject(job, JobObjectExtendedLimitInformation, &li, sizeof li);
        if (!AssignProcessToJobObject(job, pi.hProcess)) {
            CloseHandle(job);
            job = NULL;
        }
    }
    ResumeThread(pi.hThread);
    CloseHandle(pi.hThread);

    job_t *j = xcalloc(1, sizeof *j);
    j->sid = xstrdup(sid);
    j->id = xstrdup(id);
    j->name = xstrdup(name);
    j->pid = (pid_t)GetProcessId(pi.hProcess);
    j->hproc = pi.hProcess;
    j->hjob = job;
    j->h_in = in_w;
    j->h_out = out_r;
    j->h_err = er_r;
    j->in = input;
    j->in_len = in_len;
    reader_arg *a1 = xcalloc(1, sizeof *a1);
    a1->job = j; a1->h = out_r; a1->which = 1; a1->r = r;
    reader_arg *a2 = xcalloc(1, sizeof *a2);
    a2->job = j; a2->h = er_r; a2->which = 2; a2->r = r;
    j->rarg_out = a1;
    j->rarg_err = a2;
    j->th_out = CreateThread(NULL, 0, reader_thread, a1, 0, NULL);
    j->th_err = CreateThread(NULL, 0, reader_thread, a2, 0, NULL);

    size_t off = 0;
    while (off < in_len) {
        DWORD chunk = (DWORD)((in_len - off) > (1u << 20) ? (1u << 20) : (in_len - off));
        DWORD wrote = 0;
        if (!WriteFile(in_w, input + off, chunk, &wrote, NULL) || !wrote) {
            snprintf(err, errlen, "把请求写给执行器失败：错误 %lu", (unsigned long)GetLastError());
            TerminateJobObject(job, 1);
            if (j->th_out) WaitForSingleObject((HANDLE)j->th_out, 1000);
            if (j->th_err) WaitForSingleObject((HANDLE)j->th_err, 1000);
            job_free(j);
            return NULL;
        }
        off += wrote;
    }
    if (j->close_in) close_handle(&j->h_in);
    j->next = r->jobs;
    r->jobs = j;
    return j;

fail:
    if (in_r) CloseHandle(in_r);
    if (in_w) CloseHandle(in_w);
    if (out_r) CloseHandle(out_r);
    if (out_w) CloseHandle(out_w);
    if (er_r) CloseHandle(er_r);
    if (er_w) CloseHandle(er_w);
    free(input);
    return NULL;
}

static DWORD WINAPI reader_thread(LPVOID p)
{
    reader_arg *a = p;
    job_t *j = a->job;
    buf_t *b = a->which == 1 ? &j->out : &j->err;
    size_t *total = a->which == 1 ? &j->out_total : &j->err_total;
    size_t cap = a->which == 1 ? a->r->out_cap : STDERR_CAP;
    char tmp[16384];
    for (;;) {
        DWORD got = 0;
        if (!ReadFile(a->h, tmp, sizeof tmp, &got, NULL) || !got) break;
        *total += got;
        if (b->len < cap) {
            size_t room = cap - b->len;
            buf_append(b, tmp, got < room ? got : room);
        }
    }
    if (a->which == 1) InterlockedExchange(&j->out_done, 1);
    else InterlockedExchange(&j->err_done, 1);
    if (a->r->wake) plat_wake_signal(a->r->wake);
    return 0;
}

static void stop_job(job_t *j)
{
    if (j->killed) return;
    j->killed = true;
    close_handle((HANDLE *)&j->h_in);
    if (!j->exited) j->term_at = now_ms();
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
            if (j->hjob) TerminateJobObject((HANDLE)j->hjob, 1);
            else if (j->hproc) TerminateProcess((HANDLE)j->hproc, 1);
            j->term_at = 0;
        }
    }
}

void runner_reap(runner_t *r, int64_t now)
{
    for (job_t *j = r->jobs; j; j = j->next) {
        if (j->exited || !j->hproc) continue;
        if (WaitForSingleObject((HANDLE)j->hproc, 0) == WAIT_OBJECT_0) {
            DWORD code = 0;
            GetExitCodeProcess((HANDLE)j->hproc, &code);
            j->exited = true;
            j->exit_code = (int)code;
            j->exit_at = now;
            if (r->wake) plat_wake_signal(r->wake);
        }
    }
}

size_t runner_pollfds(runner_t *r, na_pollfd *pfds, pollref_t *refs, size_t max)
{
    (void)r; (void)pfds; (void)refs; (void)max;
    return 0;
}

void runner_on_poll(runner_t *r, const na_pollfd *pfds, const pollref_t *refs, size_t n)
{
    (void)r; (void)pfds; (void)refs; (void)n;
}

job_t *runner_take_finished(runner_t *r, int64_t now)
{
    for (job_t **pp = &r->jobs; *pp; pp = &(*pp)->next) {
        job_t *j = *pp;
        if (!j->exited) continue;
        bool drained = j->out_done && j->err_done;
        if (!drained && now - j->exit_at < EXIT_GRACE_MS) continue;
        if (j->hjob) TerminateJobObject((HANDLE)j->hjob, 0);
        close_handle((HANDLE *)&j->h_in);
        close_handle((HANDLE *)&j->hproc);
        close_handle((HANDLE *)&j->hjob);
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

void runner_shutdown(runner_t *r, int grace_ms)
{
    runner_kill_all(r);
    for (int64_t until = now_ms() + grace_ms; r->jobs && now_ms() < until;) {
        runner_reap(r, now_ms());
        runner_tick(r, now_ms());
        bool alive = false;
        for (job_t *j = r->jobs; j; j = j->next) alive |= !j->exited;
        if (!alive) break;
        plat_sleep_ms(20);
    }
    for (job_t *j; (j = r->jobs);) {
        if (!j->exited) {
            if (j->hjob) TerminateJobObject((HANDLE)j->hjob, 1);
            else if (j->hproc) TerminateProcess((HANDLE)j->hproc, 1);
        }
        r->jobs = j->next;
        j->next = NULL;
        job_free(j);
    }
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
    buf_t b = {0};
    char note[128];
    if (j->term_signal) snprintf(note, sizeof note, "执行器被信号 %d 终止", j->term_signal);
    else snprintf(note, sizeof note, "执行器异常结束（退出码 %d）", j->exit_code);
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
    close_handle((HANDLE *)&j->h_in);
    close_handle((HANDLE *)&j->h_out);
    close_handle((HANDLE *)&j->h_err);
    close_handle((HANDLE *)&j->hproc);
    close_handle((HANDLE *)&j->hjob);
    if (j->th_out) CloseHandle((HANDLE)j->th_out);
    if (j->th_err) CloseHandle((HANDLE)j->th_err);
    free(j->rarg_out);
    free(j->rarg_err);
    free(j->sid);
    free(j->id);
    free(j->name);
    free(j->run_id);
    free(j->res_keys);
    free(j->res_xkeys);
    free(j->sandbox);
    free(j->in);
    buf_free(&j->out);
    buf_free(&j->err);
    free(j);
}
