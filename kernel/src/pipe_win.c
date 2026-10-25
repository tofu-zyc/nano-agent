/* pipe_win.c —— 管道透传的 Windows 实现，见 pipe.h
 *
 * 主循环是 WSAPoll，等不了管道句柄，所以每条管道两个线程：读线程把 stdout 切成行塞进队列，
 * 等待线程盯着进程退出；两者都用唤醒 socket 叫醒主循环。
 * 关闭时先关 stdin，到点还不退就 TerminateJobObject 把整棵进程树杀掉。
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

/* 线程用的参数：id 是拷贝，线程结束时管道可能已经被主循环释放了 */
typedef struct {
    na_pipes *ps;
    char     *id;
    na_pipe_t *p;
    HANDLE    h;
} io_arg;

static wchar_t *to_w(const char *s)
{
    int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, NULL, 0);
    if (n <= 0) return NULL;
    wchar_t *w = xcalloc((size_t)n, sizeof *w);
    if (!MultiByteToWideChar(CP_UTF8, 0, s, -1, w, n)) { free(w); return NULL; }
    return w;
}

/* 往字符串末尾追加（自动扩容） */
typedef struct { wchar_t *p; size_t len, cap; } wbuf;

static void wput(wbuf *b, wchar_t c)
{
    if (b->len + 2 >= b->cap) {
        b->cap = b->cap ? b->cap * 2 : 256;
        b->p = xrealloc(b->p, b->cap * sizeof *b->p);
    }
    b->p[b->len++] = c;
    b->p[b->len] = 0;
}

/* 按 Windows 的规矩给一个参数加引号：空参数、带空白的、带引号的、末尾反斜杠都要对 */
static void wquote(wbuf *b, const wchar_t *arg)
{
    if (!*arg) {
        wput(b, L'"');
        wput(b, L'"');
        return;
    }
    if (!wcspbrk(arg, L" \t\n\v\"")) {
        for (const wchar_t *p = arg; *p; p++) wput(b, *p);
        return;
    }
    wput(b, L'"');
    for (const wchar_t *p = arg;; p++) {
        size_t bs = 0;
        while (*p == L'\\') { p++; bs++; }
        if (!*p) {
            for (size_t i = 0; i < bs * 2; i++) wput(b, L'\\');
            break;
        }
        if (*p == L'"') {
            for (size_t i = 0; i < bs * 2 + 1; i++) wput(b, L'\\');
            wput(b, L'"');
        } else {
            for (size_t i = 0; i < bs; i++) wput(b, L'\\');
            wput(b, *p);
        }
    }
    wput(b, L'"');
}

static wchar_t *cmdline_w(char *const argv[])
{
    wbuf b = {0};
    for (size_t i = 0; argv[i]; i++) {
        wchar_t *w = to_w(argv[i]);
        if (!w) w = to_w("");
        if (i) wput(&b, L' ');
        wquote(&b, w);
        free(w);
    }
    if (!b.p) b.p = xcalloc(1, sizeof(wchar_t));
    return b.p;
}

/* 找可执行文件：没有路径分隔符又没有扩展名时按 PATHEXT 找（npx 要能找到 npx.cmd） */
static wchar_t *resolve_exe(const char *cmd)
{
    wchar_t *w = to_w(cmd);
    if (!w) return NULL;
    if (wcschr(w, L'\\') || wcschr(w, L'/') || wcschr(w, L'.')) return w;
    wchar_t found[MAX_PATH];
    DWORD n = SearchPathW(NULL, w, L".COM;.EXE;.BAT;.CMD", MAX_PATH, found, NULL);
    if (n > 0 && n < MAX_PATH) {
        free(w);
        size_t l = wcslen(found);
        wchar_t *r = xcalloc(l + 1, sizeof *r);
        memcpy(r, found, (l + 1) * sizeof *r);
        return r;
    }
    return w;
}

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

/* 线程收尾：先把 id 交出去、把管道标成「这个线程用完了」，再释放自己的参数 */
static void io_done(io_arg *a)
{
    if (a->p) {
        pipes_lock(a->ps);
        if (a->p->live > 0) a->p->live--;
        pipes_unlock(a->ps);
    }
    if (a->h) CloseHandle(a->h);
    free(a->id);
    free(a);
}

static DWORD WINAPI reader_thread(LPVOID arg)
{
    io_arg *a = arg;
    char tmp[16384];
    buf_t line = {0};
    HANDLE h = a->h;
    for (;;) {
        DWORD got = 0;
        if (!ReadFile(h, tmp, sizeof tmp, &got, NULL) || !got) break;
        for (DWORD i = 0; i < got; i++) {
            if (tmp[i] == '\n') {
                size_t l = line.len;
                if (l && line.p[l - 1] == '\r') l--;
                pipes_queue_line(a->ps, a->id, line.p ? line.p : "", l);
                line.len = 0;
            } else {
                buf_append(&line, tmp + i, 1);
            }
        }
    }
    if (line.len) pipes_queue_line(a->ps, a->id, line.p, line.len);
    free(line.p);
    na_pipes *ps = a->ps;
    a->h = NULL;          /* io_done 里不再关它 */
    io_done(a);
    if (ps->wake) plat_wake_signal(ps->wake);
    return 0;
}

static DWORD WINAPI wait_thread(LPVOID arg)
{
    io_arg *a = arg;
    DWORD code = 0;
    WaitForSingleObject(a->h, INFINITE);
    GetExitCodeProcess(a->h, &code);
    /* 进程句柄由这里负责关：先在管道上清掉，主循环据此知道可以释放了。
       顺便让写线程收尾（进程没了，stdin 没人读了） */
    bool already = false;
    if (a->p) {
        pipes_lock(a->ps);
        a->p->hproc = NULL;
        already = a->p->reported;   /* 网关断开时已经收掉了：不用再报 */
        a->p->reported = true;
        a->p->in_eof = true;
        if (a->p->wev) SetEvent((HANDLE)a->p->wev);
        pipes_unlock(a->ps);
    }
    if (!already) pipes_queue_closed(a->ps, a->id, (int)code, "");
    na_pipes *ps = a->ps;
    io_done(a);
    if (ps->wake) plat_wake_signal(ps->wake);
    return 0;
}

#define WRITE_CAP (8u << 20)   /* 往 stdin 最多攒这么多，再多就是服务不读了 */

/* 写线程：把主循环攒下的数据写进 stdin。WriteFile 会阻塞（子进程不读的时候），所以不能在主循环里写 */
static DWORD WINAPI writer_thread(LPVOID arg)
{
    io_arg *a = arg;
    na_pipe_t *p = a->p;   /* 这个线程占着 live 的一份，p 在它结束前不会被释放 */
    for (;;) {
        WaitForSingleObject((HANDLE)p->wev, INFINITE);
        pipes_lock(a->ps);
        char *buf = p->wbuf;
        size_t len = p->wlen;
        p->wbuf = NULL;
        p->wlen = p->woff = 0;
        bool eof = p->in_eof;
        pipes_unlock(a->ps);
        bool ok = true;
        for (size_t off = 0; ok && off < len;) {
            DWORD w = 0;
            DWORD chunk = (DWORD)(len - off > (1u << 20) ? (1u << 20) : len - off);
            if (!WriteFile(a->h, buf + off, chunk, &w, NULL) || !w) ok = false;
            else off += w;
        }
        free(buf);
        if (!ok) {
            pipes_lock(a->ps);
            p->input_dead = true;
            pipes_unlock(a->ps);
            break;
        }
        if (eof) break;   /* in_eof 和缓冲是在同一把锁里取的：之前攒的都已经写完了 */
    }
    na_pipes *ps = a->ps;
    io_done(a);   /* 关掉 stdin：子进程读到 EOF */
    if (ps->wake) plat_wake_signal(ps->wake);
    return 0;
}

/* 结束一个进程：有 job 就连同它起的子进程一起杀；没有 job（加不进去）就只能杀它自己 */
static void kill_tree(na_pipes *ps, na_pipe_t *p)
{
    if (p->hjob) {
        TerminateJobObject((HANDLE)p->hjob, 1);
    } else {
        pipes_lock(ps);   /* 等待线程是在锁里清 hproc 之后才关句柄的：锁里用它是安全的 */
        if (p->hproc) TerminateProcess((HANDLE)p->hproc, 1);
        pipes_unlock(ps);
    }
    p->killed = true;
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
    HANDLE in_r = NULL, in_w = NULL, out_r = NULL, out_w = NULL, nul = INVALID_HANDLE_VALUE;
    if (!CreatePipe(&in_r, &in_w, &sa, 0) || !CreatePipe(&out_r, &out_w, &sa, 0)) {
        snprintf(err, errlen, "创建管道失败：错误 %lu", (unsigned long)GetLastError());
        return -1;
    }
    /* 父进程自己拿着的那两头不给孩子；子进程用的 in_r / out_w 必须可继承 */
    SetHandleInformation(in_w, HANDLE_FLAG_INHERIT, 0);
    SetHandleInformation(out_r, HANDLE_FLAG_INHERIT, 0);
    /* 内核自己的 stdin/stdout 传给子进程也没意义，接 NUL 更干净 */
    nul = CreateFileW(L"NUL", GENERIC_READ | GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa,
                      OPEN_EXISTING, 0, NULL);
    STARTUPINFOEXW si;
    ZeroMemory(&si, sizeof si);
    si.StartupInfo.cb = sizeof si;
    si.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    si.StartupInfo.hStdInput = in_r;
    si.StartupInfo.hStdOutput = out_w;
    si.StartupInfo.hStdError = (nul != INVALID_HANDLE_VALUE) ? nul : GetStdHandle(STD_ERROR_HANDLE);
    /* 只让子进程继承这三个句柄，别把它能碰到的东西全给它 */
    SIZE_T attr_size = 0;
    InitializeProcThreadAttributeList(NULL, 1, 0, &attr_size);
    si.lpAttributeList = xcalloc(attr_size ? attr_size : 1, 1);
    bool have_attr = attr_size && InitializeProcThreadAttributeList(si.lpAttributeList, 1, 0, &attr_size);
    if (have_attr) {
        HANDLE keep[3] = { in_r, out_w, si.StartupInfo.hStdError };
        UpdateProcThreadAttribute(si.lpAttributeList, 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
                                  keep, sizeof keep, NULL, NULL);
    } else {
        si.StartupInfo.cb = sizeof(STARTUPINFO);
    }
    wchar_t *exe = resolve_exe(argv[0]);
    wchar_t *line = cmdline_w(argv);
    wchar_t *cwdw = (cwd && *cwd) ? to_w(cwd) : NULL;
    wchar_t *envw = env_block_w(env);
    PROCESS_INFORMATION pi;
    ZeroMemory(&pi, sizeof pi);
    DWORD flags = CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT | CREATE_SUSPENDED;
    if (have_attr) flags |= EXTENDED_STARTUPINFO_PRESENT;
    BOOL ok = CreateProcessW(exe, line, NULL, NULL, TRUE, flags, envw, cwdw, &si.StartupInfo, &pi);
    DWORD e = GetLastError();
    if (have_attr) DeleteProcThreadAttributeList(si.lpAttributeList);
    free(si.lpAttributeList);
    free(exe); free(line); free(cwdw); free(envw);
    CloseHandle(in_r);
    CloseHandle(out_w);
    if (nul != INVALID_HANDLE_VALUE) CloseHandle(nul);
    if (!ok) {
        CloseHandle(in_w); CloseHandle(out_r);
        snprintf(err, errlen, "无法启动 %s：错误 %lu", argv[0], (unsigned long)e);
        return -1;
    }
    /* 还挂起着的时候先放进 job，再让它跑：不然 npx 这类先起子进程的，子进程可能落在 job 外面，杀不掉 */
    HANDLE job = CreateJobObjectW(NULL, NULL);
    if (job) {
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION li;
        ZeroMemory(&li, sizeof li);
        li.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        SetInformationJobObject(job, JobObjectExtendedLimitInformation, &li, sizeof li);
        if (!AssignProcessToJobObject(job, pi.hProcess)) { CloseHandle(job); job = NULL; }
    }
    HANDLE wev = CreateEventW(NULL, FALSE, FALSE, NULL);
    if (!wev) {
        e = GetLastError();
        TerminateProcess(pi.hProcess, 1);
        CloseHandle(pi.hThread); CloseHandle(pi.hProcess);
        if (job) CloseHandle(job);
        CloseHandle(in_w); CloseHandle(out_r);
        snprintf(err, errlen, "创建事件失败：错误 %lu", (unsigned long)e);
        return -1;
    }
    ResumeThread(pi.hThread);
    CloseHandle(pi.hThread);
    na_pipe_t *p = xcalloc(1, sizeof *p);
    p->id = xstrdup(id);
    p->hproc = pi.hProcess;
    p->hjob = job;
    p->wev = wev;
    p->live = 3;   /* 读线程 + 等待线程 + 写线程 */
    p->next = ps->list;
    ps->list = p;
    io_arg *a1 = xcalloc(1, sizeof *a1);
    a1->ps = ps; a1->id = xstrdup(id); a1->p = p; a1->h = out_r;
    if (!CreateThread(NULL, 0, reader_thread, a1, 0, NULL)) { a1->h = NULL; io_done(a1); CloseHandle(out_r); }
    io_arg *a2 = xcalloc(1, sizeof *a2);
    a2->ps = ps; a2->id = xstrdup(id); a2->p = p; a2->h = pi.hProcess;
    if (!CreateThread(NULL, 0, wait_thread, a2, 0, NULL)) { a2->h = NULL; io_done(a2); }
    io_arg *a3 = xcalloc(1, sizeof *a3);
    a3->ps = ps; a3->id = xstrdup(id); a3->p = p; a3->h = in_w;
    if (!CreateThread(NULL, 0, writer_thread, a3, 0, NULL)) { p->input_dead = true; io_done(a3); }
    return 0;
}

int pipes_write(na_pipes *ps, const char *id, const char *data)
{
    na_pipe_t *p = find_pipe(ps, id);
    size_t n = strlen(data ? data : "");
    const char *why = NULL;
    pipes_lock(ps);
    if (!p) why = "没有这条管道";
    else if (p->input_dead || p->in_eof || p->reported) why = "进程不再读 stdin 了";
    else if (p->wlen + n + 1 > WRITE_CAP) {
        why = "写给 stdin 的数据太多，服务好像不读了";
        p->in_eof = true;
        SetEvent((HANDLE)p->wev);
    } else {
        p->wbuf = xrealloc(p->wbuf, p->wlen + n + 1);
        if (n) memcpy(p->wbuf + p->wlen, data, n);
        p->wbuf[p->wlen + n] = '\n';
        p->wlen += n + 1;
        SetEvent((HANDLE)p->wev);   /* 写线程去写；主循环不等 */
    }
    pipes_unlock(ps);
    if (!why) return 0;
    /* 网关还当它活着：回一条退出，别让它一直等；进程可能还活着，按关闭处理，到点不退就杀 */
    pipes_queue_closed(ps, id ? id : "", -1, why);
    if (p && !p->closing) {
        p->closing = true;
        p->close_at = now_ms();
    }
    return -1;
}

void pipes_close(na_pipes *ps, const char *id)
{
    na_pipe_t *p = find_pipe(ps, id);
    if (!p) {
        pipes_queue_closed(ps, id ? id : "", -1, "没有这条管道");
        return;
    }
    pipes_lock(ps);
    p->in_eof = true;   /* 写线程把攒下的写完就关 stdin */
    SetEvent((HANDLE)p->wev);
    pipes_unlock(ps);
    p->closing = true;
    p->close_at = now_ms();
}

size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_piperef *refs, size_t max)
{
    (void)ps; (void)pfds; (void)refs; (void)max;
    return 0;   /* Windows 上用读线程，不占主循环的 poll */
}

void pipes_poll(na_pipes *ps, const na_pollfd *pfds, const na_piperef *refs, size_t n, int64_t now)
{
    (void)pfds; (void)refs; (void)n;
    for (na_pipe_t *p = ps->list; p; p = p->next) {
        if (p->closing && !p->killed && now - p->close_at > ps->close_grace_ms) kill_tree(ps, p);
    }
    /* 报过退出、三个线程也都用完了的，清掉 */
    na_pipe_t **pp = &ps->list;
    while (*pp) {
        na_pipe_t *p = *pp;
        pipes_lock(ps);
        bool done = p->reported && p->live <= 0 && !p->hproc;
        pipes_unlock(ps);
        if (done) {
            *pp = p->next;
            if (p->hjob) CloseHandle((HANDLE)p->hjob);
            if (p->wev) CloseHandle((HANDLE)p->wev);
            free_pipe(p);
            continue;
        }
        pp = &p->next;
    }
}

void pipes_reset(na_pipes *ps)
{
    for (na_pipe_t *p = ps->list; p; p = p->next) {
        pipes_lock(ps);
        p->in_eof = true;
        if (p->wev) SetEvent((HANDLE)p->wev);
        p->reported = true;   /* 网关已经不在了：进程退出时不用再报 */
        pipes_unlock(ps);
        if (!p->killed) kill_tree(ps, p);
    }
    pipes_clear_queue(ps);
    /* 条目等三个线程都结束后由 pipes_poll 释放（线程还拿着它们） */
}

void pipes_shutdown(na_pipes *ps)
{
    /* 内核要退出了：杀掉就行，进程退出时线程跟着没了，不用等它们 */
    pipes_reset(ps);
}
