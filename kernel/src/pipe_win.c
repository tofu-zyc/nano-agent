/* pipe_win.c —— 管道透传的 Windows 实现，见 pipe.h
 *
 * 主循环是 WSAPoll，等不了管道句柄，所以每条管道两个线程：读线程把 stdout 切成行塞进队列，
 * 等待线程盯着进程退出，两者都用唤醒 socket 叫醒主循环。关闭时先关 stdin，
 * 到点还不退就 TerminateJobObject 把整棵进程树杀掉。
 */
#define WIN32_LEAN_AND_MEAN
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0A00
#endif
#include <winsock2.h>
#include <windows.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "pipe.h"
#include "util.h"

typedef struct {
    na_pipes *ps;
    na_pipe_t *p;
    HANDLE h;
} read_arg;

static wchar_t *to_w(const char *s)
{
    int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, NULL, 0);
    if (n <= 0) return NULL;
    wchar_t *w = xcalloc((size_t)n, sizeof *w);
    if (!MultiByteToWideChar(CP_UTF8, 0, s, -1, w, n)) { free(w); return NULL; }
    return w;
}

/* 命令行：第一个参数带空格就加引号 */
static wchar_t *cmdline_w(char *const argv[])
{
    size_t cap = 256, len = 0;
    wchar_t *out = xcalloc(cap, sizeof *out);
    for (size_t i = 0; argv[i]; i++) {
        wchar_t *w = to_w(argv[i]);
        if (!w) w = to_w("");
        size_t need = wcslen(w) + 4;
        while (len + need >= cap) {
            cap *= 2;
            out = xrealloc(out, cap * sizeof *out);
            memset(out + len, 0, (cap - len) * sizeof *out);
        }
        if (i) out[len++] = L' ';
        bool quote = wcschr(w, L' ') != NULL;
        if (quote) out[len++] = L'"';
        memcpy(out + len, w, wcslen(w) * sizeof *w);
        len += wcslen(w);
        if (quote) out[len++] = L'"';
        free(w);
    }
    return out;
}

/* 环境块：父进程的 + 网关给的那些（同名覆盖），双 NUL 结尾 */
static wchar_t *env_block_w(const char *const env[])
{
    wchar_t *parent = GetEnvironmentStringsW();
    size_t cap = 4096, n = 0;
    wchar_t *out = xcalloc(cap, sizeof *out);
    if (parent) {
        for (wchar_t *s = parent; *s; s += wcslen(s) + 1) {
            size_t l = wcslen(s);
            while (n + l + 2 >= cap) {
                cap *= 2;
                out = xrealloc(out, cap * sizeof *out);
                memset(out + n, 0, (cap - n) * sizeof *out);
            }
            memcpy(out + n, s, l * sizeof *s);
            n += l + 1;
        }
        FreeEnvironmentStringsW(parent);
    }
    for (size_t i = 0; env && env[i] && env[i + 1]; i += 2) {
        wchar_t *k = to_w(env[i]), *v = to_w(env[i + 1]);
        if (!k || !v) { free(k); free(v); continue; }
        size_t need = wcslen(k) + wcslen(v) + 3;
        while (n + need >= cap) {
            cap *= 2;
            out = xrealloc(out, cap * sizeof *out);
            memset(out + n, 0, (cap - n) * sizeof *out);
        }
        memcpy(out + n, k, wcslen(k) * sizeof *k);
        n += wcslen(k);
        out[n++] = L'=';
        memcpy(out + n, v, wcslen(v) * sizeof *v);
        n += wcslen(v);
        out[n++] = 0;
        free(k);
        free(v);
    }
    out[n] = 0;
    return out;
}

static DWORD WINAPI reader_thread(LPVOID arg)
{
    read_arg *a = arg;
    char tmp[16384];
    buf_t line = {0};
    for (;;) {
        DWORD got = 0;
        if (!ReadFile(a->h, tmp, sizeof tmp, &got, NULL) || !got) break;
        for (DWORD i = 0; i < got; i++) {
            if (tmp[i] == '\n') {
                size_t l = line.len;
                if (l && line.p[l - 1] == '\r') l--;
                pipes_queue_line(a->ps, a->p->id, line.p ? line.p : "", l);
                line.len = 0;
            } else {
                buf_append(&line, tmp + i, 1);
            }
        }
    }
    if (line.len) pipes_queue_line(a->ps, a->p->id, line.p, line.len);
    free(line.p);
    CloseHandle(a->h);
    free(a);
    if (a->ps->wake) plat_wake_signal(a->ps->wake);
    return 0;
}

static DWORD WINAPI wait_thread(LPVOID arg)
{
    read_arg *a = arg;
    WaitForSingleObject(a->h, INFINITE);
    DWORD code = 0;
    GetExitCodeProcess(a->h, &code);
    pipes_queue_closed(a->ps, a->p->id, (int)code, "");
    CloseHandle(a->h);
    free(a);
    if (a->ps->wake) plat_wake_signal(a->ps->wake);
    return 0;
}

void pipes_lock(na_pipes *ps) { if (ps->lock) EnterCriticalSection((CRITICAL_SECTION *)ps->lock); }
void pipes_unlock(na_pipes *ps) { if (ps->lock) LeaveCriticalSection((CRITICAL_SECTION *)ps->lock); }

void pipes_init(na_pipes *ps)
{
    memset(ps, 0, sizeof *ps);
    ps->close_grace_ms = 3000;
    CRITICAL_SECTION *cs = xcalloc(1, sizeof *cs);
    InitializeCriticalSection(cs);
    ps->lock = cs;
}

void pipes_set_wake(na_pipes *ps, na_wake *w) { ps->wake = w; }

int pipes_open(na_pipes *ps, const char *id, char *const argv[], const char *const env[], const char *cwd,
               char *err, size_t errlen)
{
    if (!argv || !argv[0]) { snprintf(err, errlen, "没有要跑的命令"); return -1; }
    SECURITY_ATTRIBUTES sa = { sizeof sa, NULL, TRUE };
    HANDLE in_r = NULL, in_w = NULL, out_r = NULL, out_w = NULL;
    if (!CreatePipe(&in_r, &in_w, &sa, 0) || !CreatePipe(&out_r, &out_w, &sa, 0)) {
        snprintf(err, errlen, "创建管道失败：错误 %lu", (unsigned long)GetLastError());
        return -1;
    }
    SetHandleInformation(in_w, HANDLE_FLAG_INHERIT, 0);
    SetHandleInformation(out_r, HANDLE_FLAG_INHERIT, 0);
    STARTUPINFOW si;
    ZeroMemory(&si, sizeof si);
    si.cb = sizeof si;
    si.dwFlags = STARTF_USESTDHANDLES;
    si.hStdInput = in_r;
    si.hStdOutput = out_w;
    /* stderr 接到 NUL：内核的 stderr 句柄不可继承，直接传会让子进程拿到坏句柄 */
    HANDLE nul = CreateFileW(L"NUL", GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa, OPEN_EXISTING, 0, NULL);
    si.hStdError = (nul != INVALID_HANDLE_VALUE) ? nul : GetStdHandle(STD_ERROR_HANDLE);
    wchar_t *line = cmdline_w(argv);
    wchar_t *cwdw = (cwd && *cwd) ? to_w(cwd) : NULL;
    wchar_t *envw = env_block_w(env);
    PROCESS_INFORMATION pi;
    ZeroMemory(&pi, sizeof pi);
    BOOL ok = CreateProcessW(NULL, line, NULL, NULL, TRUE,
                             CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT, envw, cwdw, &si, &pi);
    DWORD e = GetLastError();
    free(line); free(cwdw); free(envw);
    CloseHandle(in_r);
    CloseHandle(out_w);
    if (nul != INVALID_HANDLE_VALUE) CloseHandle(nul);
    if (!ok) {
        CloseHandle(in_w); CloseHandle(out_r);
        snprintf(err, errlen, "无法启动 %s：错误 %lu", argv[0], (unsigned long)e);
        return -1;
    }
    CloseHandle(pi.hThread);
    HANDLE job = CreateJobObjectW(NULL, NULL);
    if (job) {
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION li;
        ZeroMemory(&li, sizeof li);
        li.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        SetInformationJobObject(job, JobObjectExtendedLimitInformation, &li, sizeof li);
        if (!AssignProcessToJobObject(job, pi.hProcess)) { CloseHandle(job); job = NULL; }
    }
    na_pipe_t *p = xcalloc(1, sizeof *p);
    p->id = xstrdup(id);
    p->hproc = pi.hProcess;
    p->hjob = job;
    p->h_in = in_w;
    p->h_out = out_r;
    p->next = ps->list;
    ps->list = p;
    read_arg *a1 = xcalloc(1, sizeof *a1);
    a1->ps = ps; a1->p = p; a1->h = out_r;
    CreateThread(NULL, 0, reader_thread, a1, 0, NULL);
    read_arg *a2 = xcalloc(1, sizeof *a2);
    a2->ps = ps; a2->p = p; a2->h = pi.hProcess;
    CreateThread(NULL, 0, wait_thread, a2, 0, NULL);
    return 0;
}

int pipes_write(na_pipes *ps, const char *id, const char *data)
{
    na_pipe_t *p = find_pipe(ps, id);
    if (!p || !p->h_in) return -1;
    size_t n = strlen(data ? data : "");
    char *line = xcalloc(n + 2, 1);
    memcpy(line, data ? data : "", n);
    line[n] = '\n';
    DWORD wrote = 0;
    BOOL ok = WriteFile((HANDLE)p->h_in, line, (DWORD)(n + 1), &wrote, NULL);
    free(line);
    return ok ? 0 : -1;
}

void pipes_close(na_pipes *ps, const char *id)
{
    na_pipe_t *p = find_pipe(ps, id);
    if (!p) return;
    if (p->h_in) { CloseHandle((HANDLE)p->h_in); p->h_in = NULL; }
    p->closing = true;
    p->close_at = now_ms();
}

size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_pipe_t **refs, size_t max)
{
    (void)ps; (void)pfds; (void)refs; (void)max;
    return 0;   /* Windows 上用读线程，不占主循环的 poll */
}

void pipes_poll(na_pipes *ps, const na_pollfd *pfds, na_pipe_t **refs, size_t n, int64_t now)
{
    (void)pfds; (void)refs; (void)n;
    for (na_pipe_t *p = ps->list; p; p = p->next) {
        if (!p->closing || !p->hjob || p->killed) continue;
        if (now - p->close_at > ps->close_grace_ms) {
            TerminateJobObject((HANDLE)p->hjob, 1);
            p->killed = true;
        }
    }
}

void pipes_shutdown(na_pipes *ps)
{
    for (na_pipe_t *p = ps->list; p; p = p->next) {
        if (p->h_in) { CloseHandle((HANDLE)p->h_in); p->h_in = NULL; }
        if (p->hjob) { TerminateJobObject((HANDLE)p->hjob, 1); CloseHandle((HANDLE)p->hjob); p->hjob = NULL; }
        if (p->hproc) { CloseHandle((HANDLE)p->hproc); p->hproc = NULL; }
    }
}
