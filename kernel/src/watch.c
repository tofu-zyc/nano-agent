#include "watch.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#if defined(__linux__)
#include <sys/inotify.h>
#include <fcntl.h>
#include <unistd.h>
#elif defined(__APPLE__)
#include <sys/event.h>
#include <sys/time.h>
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <unistd.h>
#elif defined(_WIN32)
#include <winsock2.h>
#include <ws2tcpip.h>
#include <windows.h>
#endif

#define WATCH_PATH_MAX 1024
#define WATCH_KEY_MAX  96
#define NA_WATCH_PENDING 256 /* 工作线程到主线程的待处理事件上限（超出按缺口处理） */

typedef struct {
    char    key[WATCH_KEY_MAX];
    char    path[WATCH_PATH_MAX];
    char    last[WATCH_PATH_MAX]; /* 最近一次变化的实际路径（目录登记时是里面的文件） */
    int     wd;                   /* Linux: inotify watch；macOS: 文件 fd；Windows: 槽位 */
    unsigned gen;                 /* 每次重新挂接递增：旧的异步事件按它丢弃 */
    bool    used;                 /* 槽位在用（槽位身份稳定：删登记不搬动别的条目） */
    int     count;
    int64_t due;                  /* 0 表示没有待发的事件 */
    int64_t next;                 /* 观察不到时下次再试的时刻 */
    bool    resync;
} watch_entry;

/* 工作线程交给主循环的一条观察结果（Linux 没有工作线程，用不到）。 */
typedef struct {
    int      slot;
    unsigned gen;
    char     name[256];
} pending_ev;

struct na_watch {
    intptr_t    fd;   /* 主循环轮询的句柄：Linux inotify / macOS 管道读端 / Windows UDP 套接字 */
    int         n;
    watch_entry items[NA_WATCH_MAX];
#if !defined(__linux__)
    pending_ev  pend[NA_WATCH_PENDING];
    int         pend_head, pend_tail;
    bool        overflow;   /* 队列满过：所有登记各报一次缺口 */
    unsigned    gen;
#if defined(__APPLE__)
    int         kq;         /* kqueue */
    int         fds[NA_WATCH_MAX]; /* 每个槽位的 vnode fd（只由工作线程读写） */
    int         cmd[2];     /* 命令管道：主线程 → 工作线程 */
    int         wake[2];    /* 唤醒管道：工作线程 → 主线程（wake[0] 就是 fd） */
    pthread_t   thread;
    pthread_mutex_t lock;
    pthread_cond_t  cond;
    bool        thread_started, stop;
    struct { int kind; watch_entry *e; } cmdq[NA_WATCH_MAX]; /* 0 空 / 1 挂接 / 2 摘除 */
    int         cmd_head, cmd_tail;
#elif defined(_WIN32)
    HANDLE      iocp;       /* 完成端口：目录变化完成和命令都从这里出来 */
    HANDLE      thread;
    SOCKET      wake_sock;  /* 同一个套接字既轮询又用来唤醒 */
    struct sockaddr_in wake_addr;
    SRWLOCK     lock;
    bool        thread_started, stop;
    struct { int kind; watch_entry *e; } cmdq[NA_WATCH_MAX];
    int         cmd_head, cmd_tail;
    struct win_watch *ww[NA_WATCH_MAX]; /* 每个槽位的目录句柄和重叠结构 */
#endif
#endif
};

static const char *sget(const cJSON *o, const char *k)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(o, k);
    return cJSON_IsString(v) ? v->valuestring : NULL;
}

/* ---------- 公共：待处理事件队列（macOS / Windows 的工作线程用） ---------- */
#if !defined(__linux__)
static void queue_push(na_watch *w, int slot, unsigned gen, const char *name)
{
    int next = (w->pend_tail + 1) % NA_WATCH_PENDING;
    if (next == w->pend_head) {
        w->overflow = true; /* 满过：给所有登记各报一次缺口，不丢"这里可能变了"的事实 */
        return;
    }
    pending_ev *ev = &w->pend[w->pend_tail];
    ev->slot = slot;
    ev->gen = gen;
    ev->name[0] = 0;
    if (name && *name) snprintf(ev->name, sizeof ev->name, "%s", name);
    w->pend_tail = next;
}

static int queue_pop(na_watch *w, pending_ev *out)
{
    if (w->pend_head == w->pend_tail) return 0;
    *out = w->pend[w->pend_head];
    w->pend_head = (w->pend_head + 1) % NA_WATCH_PENDING;
    return 1;
}
#endif

#if !defined(__linux__)
static watch_entry *watch_by_slot(na_watch *w, int slot)
{
    if (slot < 0 || slot >= w->n || !w->items[slot].used) return NULL;
    return &w->items[slot];
}
#endif

/* 目录登记时把里面的文件名接到后面；超长就截断，报告用的路径不因此丢失。 */
static void join_path(char *out, size_t cap, const char *dir, const char *name)
{
    size_t d = strlen(dir);
    if (d > cap - 1) d = cap - 1;
    memcpy(out, dir, d);
    out[d] = 0;
    if (!name || !*name || d + 2 > cap) return;
    size_t room = cap - d - 2, n = strlen(name);
    if (n > room) n = room;
    out[d] = '/';
    memcpy(out + d + 1, name, n);
    out[d + 1 + n] = 0;
}

/* ================= Linux：inotify（不额外起线程） ================= */
#if defined(__linux__)
#define WATCH_MASK (IN_MODIFY | IN_ATTRIB | IN_CLOSE_WRITE | IN_CREATE | IN_DELETE | \
                    IN_MOVED_FROM | IN_MOVED_TO | IN_DELETE_SELF | IN_MOVE_SELF)

static watch_entry *watch_by_wd(na_watch *w, int wd)
{
    for (int i = 0; i < w->n; i++)
        if (w->items[i].wd >= 0 && w->items[i].wd == wd) return &w->items[i];
    return NULL;
}

static int be_init(na_watch *w)
{
    w->fd = inotify_init1(IN_NONBLOCK | IN_CLOEXEC);
    return w->fd >= 0 ? 0 : -1;
}

static void be_free(na_watch *w)
{
    if (w->fd >= 0) close(w->fd);
    w->fd = -1;
}

/* 失败即返回 -1：路径不在、权限不够都算"现在观察不到"，由调用处按缺口报一次。 */
static int be_attach(na_watch *w, watch_entry *e)
{
    if (w->fd < 0) return -1;
    return inotify_add_watch(w->fd, e->path, WATCH_MASK);
}

static void be_detach(na_watch *w, watch_entry *e)
{
    if (w->fd >= 0 && e->wd >= 0) inotify_rm_watch(w->fd, e->wd);
    e->wd = -1;
}

static void be_ready(na_watch *w, int64_t now)
{
    if (w->fd < 0) return;
    int64_t due = now + NA_WATCH_DEBOUNCE;
    union { struct inotify_event ev; char buf[8192]; } u;
    for (int reads = 0; reads < 16; reads++) {
        ssize_t n = read(w->fd, u.buf, sizeof u.buf);
        if (n <= 0) return;
        for (char *p = u.buf; p < u.buf + n;) {
            const struct inotify_event *ev = (const struct inotify_event *)p;
            p += sizeof(struct inotify_event) + ev->len;
            if (ev->mask & IN_Q_OVERFLOW) {
                for (int i = 0; i < w->n; i++) {
                    w->items[i].resync = true;
                    w->items[i].due = due;
                }
                continue;
            }
            watch_entry *e = watch_by_wd(w, ev->wd);
            if (!e) continue;
            if (ev->mask & (IN_IGNORED | IN_DELETE_SELF | IN_MOVE_SELF | IN_UNMOUNT)) {
                e->wd = -1;                 /* 路径没了：按缺口报一次，之后定期重试 */
                e->next = now + NA_WATCH_RETRY;
                e->resync = true;
            }
            char full[WATCH_PATH_MAX];
            join_path(full, sizeof full, e->path, ev->len ? ev->name : NULL);
            memcpy(e->last, full, sizeof e->last);
            e->count++;
            e->due = due;
        }
    }
}
#endif /* __linux__ */


/* ================= macOS：kqueue + 工作线程 + 管道唤醒 =================
 * 主循环只轮询 wake[0]（普通管道，poll 能等）；登记的 vnode fd 由唯一的工作线程
 * 放进同一个 kqueue，事件先进有界队列再唤醒主循环。线程数和路径数无关。 */
#if defined(__APPLE__)
static void mac_wake(na_watch *w)
{
    char b = 1;
    ssize_t n = write(w->wake[1], &b, 1);
    (void)n; /* 管道满说明主循环马上会醒，忽略 */
}

/* 工作线程里真正挂接；失败时把槽位标回"没挂上"，主循环五秒后重试。 */
static void mac_open(na_watch *w, watch_entry *e)
{
    int slot = (int)(e - w->items);
    if (w->fds[slot] >= 0) return;
    int fd = open(e->path, O_EVTONLY);
    if (fd < 0) {
        pthread_mutex_lock(&w->lock);
        e->wd = -1;
        e->next = (int64_t)time(NULL) + NA_WATCH_RETRY / 1000;
        pthread_mutex_unlock(&w->lock);
        return;
    }
    struct kevent reg;
    EV_SET(&reg, (uintptr_t)fd, EVFILT_VNODE, EV_ADD | EV_CLEAR,
           NOTE_WRITE | NOTE_DELETE | NOTE_RENAME | NOTE_EXTEND | NOTE_ATTRIB | NOTE_REVOKE, 0,
           (void *)(uintptr_t)(((uintptr_t)e->gen << 8) | (uintptr_t)slot));
    if (kevent(w->kq, &reg, 1, NULL, 0, NULL) < 0) {
        close(fd);
        pthread_mutex_lock(&w->lock);
        e->wd = -1;
        e->next = (int64_t)time(NULL) + NA_WATCH_RETRY / 1000;
        pthread_mutex_unlock(&w->lock);
        return;
    }
    w->fds[slot] = fd;
    pthread_mutex_lock(&w->lock);
    e->wd = 1; /* 已挂上（真实 fd 在工作线程自己的表里） */
    e->next = 0;
    pthread_mutex_unlock(&w->lock);
}

static void mac_close(na_watch *w, watch_entry *e)
{
    int slot = (int)(e - w->items);
    if (w->fds[slot] < 0) return;
    struct kevent reg;
    EV_SET(&reg, (uintptr_t)w->fds[slot], EVFILT_VNODE, EV_DELETE, 0, 0, NULL);
    if (w->kq >= 0) kevent(w->kq, &reg, 1, NULL, 0, NULL);
    close(w->fds[slot]);
    w->fds[slot] = -1;
}

static void *mac_thread(void *ud)
{
    na_watch *w = ud;
    struct kevent evs[32];
    for (;;) {
        pthread_mutex_lock(&w->lock);
        bool stop = w->stop;
        pthread_mutex_unlock(&w->lock);
        if (stop) break;
        int n = kevent(w->kq, NULL, 0, evs, 32, NULL); /* 命令管道或文件事件都会唤醒 */
        if (n < 0) {
            if (errno == EINTR) continue;
            break;
        }
        for (int i = 0; i < n; i++) {
            struct kevent *ev = &evs[i];
            if (ev->filter == EVFILT_READ && ev->ident == (uintptr_t)w->cmd[0]) {
                char b[64];
                while (read(w->cmd[0], b, sizeof b) > 0) { }
                for (;;) {
                    pthread_mutex_lock(&w->lock);
                    if (w->cmd_head == w->cmd_tail) { pthread_mutex_unlock(&w->lock); break; }
                    int kind = w->cmdq[w->cmd_head].kind;
                    watch_entry *e = w->cmdq[w->cmd_head].e;
                    w->cmd_head = (w->cmd_head + 1) % NA_WATCH_MAX;
                    pthread_mutex_unlock(&w->lock);
                    if (kind == 1) mac_open(w, e);
                    else if (kind == 2) mac_close(w, e);
                }
                continue;
            }
            if (ev->filter != EVFILT_VNODE) continue;
            uintptr_t v = (uintptr_t)ev->udata;
            int slot = (int)(v & 0xFFu);
            unsigned gen = (unsigned)(v >> 8);
            pthread_mutex_lock(&w->lock);
            queue_push(w, slot, gen, NULL);
            pthread_mutex_unlock(&w->lock);
            mac_wake(w);
        }
    }
    return NULL;
}

static int be_init(na_watch *w)
{
    for (int i = 0; i < NA_WATCH_MAX; i++) w->fds[i] = -1;
    w->kq = kqueue();
    if (w->kq < 0) return -1;
    if (pipe(w->cmd) != 0) { close(w->kq); w->kq = -1; return -1; }
    if (pipe(w->wake) != 0) {
        close(w->kq); w->kq = -1;
        close(w->cmd[0]); close(w->cmd[1]);
        return -1;
    }
    fcntl(w->cmd[0], F_SETFL, O_NONBLOCK);
    fcntl(w->wake[0], F_SETFL, O_NONBLOCK);
    fcntl(w->wake[1], F_SETFL, O_NONBLOCK);
    pthread_mutex_init(&w->lock, NULL);
    pthread_cond_init(&w->cond, NULL);
    struct kevent reg;
    EV_SET(&reg, (uintptr_t)w->cmd[0], EVFILT_READ, EV_ADD, 0, 0, NULL);
    if (kevent(w->kq, &reg, 1, NULL, 0, NULL) < 0) {
        close(w->kq); w->kq = -1;
        close(w->cmd[0]); close(w->cmd[1]);
        close(w->wake[0]); close(w->wake[1]);
        return -1;
    }
    w->fd = w->wake[0];
    if (pthread_create(&w->thread, NULL, mac_thread, w) != 0) {
        close(w->kq); w->kq = -1;
        close(w->cmd[0]); close(w->cmd[1]);
        close(w->wake[0]); close(w->wake[1]);
        w->fd = -1;
        return -1;
    }
    w->thread_started = true;
    return 0;
}

static void be_free(na_watch *w)
{
    if (w->thread_started) {
        pthread_mutex_lock(&w->lock);
        w->stop = true;
        pthread_mutex_unlock(&w->lock);
        char b = 1;
        ssize_t n = write(w->cmd[1], &b, 1);
        (void)n;
        pthread_join(w->thread, NULL);
        w->thread_started = false;
    }
    for (int i = 0; i < w->n; i++) mac_close(w, &w->items[i]);
    if (w->kq >= 0) close(w->kq);
    if (w->cmd[0] >= 0) close(w->cmd[0]);
    if (w->cmd[1] >= 0) close(w->cmd[1]);
    if (w->wake[1] >= 0) close(w->wake[1]);
    if (w->fd >= 0) close(w->fd);
    w->kq = -1;
    w->cmd[0] = w->cmd[1] = w->wake[0] = w->wake[1] = -1;
    w->fd = -1;
    pthread_mutex_destroy(&w->lock);
    pthread_cond_destroy(&w->cond);
}

/* 主线程：登记/摘除都排队给工作线程，避免两边同时改 kqueue。 */
static int be_attach(na_watch *w, watch_entry *e)
{
    e->gen = ++w->gen;
    pthread_mutex_lock(&w->lock);
    w->cmdq[w->cmd_tail].kind = 1;
    w->cmdq[w->cmd_tail].e = e;
    w->cmd_tail = (w->cmd_tail + 1) % NA_WATCH_MAX;
    pthread_mutex_unlock(&w->lock);
    char b = 1;
    ssize_t n = write(w->cmd[1], &b, 1);
    (void)n;
    e->wd = 1; /* 已交给工作线程；真正的 fd 由工作线程持有 */
    return 1;
}

static void be_detach(na_watch *w, watch_entry *e)
{
    if (e->wd < 0) return;
    pthread_mutex_lock(&w->lock);
    w->cmdq[w->cmd_tail].kind = 2;
    w->cmdq[w->cmd_tail].e = e;
    w->cmd_tail = (w->cmd_tail + 1) % NA_WATCH_MAX;
    pthread_mutex_unlock(&w->lock);
    char b = 1;
    ssize_t n = write(w->cmd[1], &b, 1);
    (void)n;
    e->wd = -1;
}

static void be_ready(na_watch *w, int64_t now)
{
    if (w->fd < 0) return;
    char b[256];
    while (read(w->fd, b, sizeof b) > 0) { } /* 清掉唤醒字节 */
    int64_t due = now + NA_WATCH_DEBOUNCE;
    pthread_mutex_lock(&w->lock);
    bool overflow = w->overflow;
    if (overflow) w->pend_head = w->pend_tail = 0;
    w->overflow = false;
    pending_ev ev;
    while (queue_pop(w, &ev)) {
        watch_entry *e = watch_by_slot(w, ev.slot);
        if (!e || e->gen != ev.gen) continue; /* 已经重新挂接过的旧事件 */
        char full[WATCH_PATH_MAX];
        join_path(full, sizeof full, e->path, ev.name);
        memcpy(e->last, full, sizeof e->last);
        e->count++;
        e->due = due;
    }
    pthread_mutex_unlock(&w->lock);
    if (overflow) {
        for (int i = 0; i < w->n; i++) {
            w->items[i].resync = true;
            w->items[i].due = due;
        }
    }
}
#endif /* __APPLE__ */

#if defined(_WIN32)
/* ================= Windows：一个 IOCP 工作线程 + ReadDirectoryChangesW =================
 * 每条登记在目录句柄上挂一个重叠读，完成事件和命令都从同一个完成端口出来；
 * 主循环只轮询一个回环 UDP 套接字（WSAPoll 认套接字），不会被观察 I/O 阻塞。
 * 线程只有一个，和路径数无关。 */
typedef struct win_watch {
    OVERLAPPED ov;
    HANDLE     dir;
    int        slot;
    unsigned   gen;
    char       name[256]; /* 明确文件登记时只认这个名字；目录登记为空 */
    bool       dead, pending;
    BYTE       buf[4096];
} win_watch;

#define WIN_KEY_STOP ((ULONG_PTR)1)
#define WIN_KEY_CMD  ((ULONG_PTR)2)

static void be_detach(na_watch *w, watch_entry *e);

static void win_wake(na_watch *w)
{
    sendto(w->wake_sock, "x", 1, 0, (struct sockaddr *)&w->wake_addr, sizeof w->wake_addr);
}

static void win_notify(na_watch *w, win_watch *ww, const char *name)
{
    AcquireSRWLockExclusive(&w->lock);
    queue_push(w, ww->slot, ww->gen, name);
    ReleaseSRWLockExclusive(&w->lock);
    win_wake(w);
}

static void win_free_ww(na_watch *w, win_watch *ww)
{
    if (ww->dir) CloseHandle(ww->dir);
    if (ww->slot >= 0 && ww->slot < NA_WATCH_MAX && w->ww[ww->slot] == ww) w->ww[ww->slot] = NULL;
    free(ww);
}

static bool win_issue(win_watch *ww)
{
    memset(&ww->ov, 0, sizeof ww->ov);
    BOOL ok = ReadDirectoryChangesW(ww->dir, ww->buf, sizeof ww->buf, FALSE,
                                    FILE_NOTIFY_CHANGE_FILE_NAME | FILE_NOTIFY_CHANGE_DIR_NAME |
                                        FILE_NOTIFY_CHANGE_LAST_WRITE | FILE_NOTIFY_CHANGE_SIZE |
                                        FILE_NOTIFY_CHANGE_ATTRIBUTES,
                                    NULL, &ww->ov, NULL);
    ww->pending = ok != 0;
    return ww->pending;
}

/* 把一条完成里带的文件名按 UTF-8 进队列。 */
static void win_scan(na_watch *w, win_watch *ww, DWORD bytes)
{
    if (!bytes) {
        win_notify(w, ww, NULL);
        return;
    }
    BYTE *p = ww->buf;
    bool any = false;
    for (;;) {
        FILE_NOTIFY_INFORMATION *info = (FILE_NOTIFY_INFORMATION *)p;
        int chars = (int)(info->FileNameLength / sizeof(WCHAR));
        char name[256];
        name[0] = 0;
        if (chars > 0) {
            int need = WideCharToMultiByte(CP_UTF8, 0, info->FileName, chars, NULL, 0, NULL, NULL);
            if (need > 0) {
                if (need >= (int)sizeof name) need = (int)sizeof name - 1;
                WideCharToMultiByte(CP_UTF8, 0, info->FileName, chars, name, need, NULL, NULL);
                name[need] = 0;
            }
        }
        bool match = ww->name[0] == 0 || _stricmp(ww->name, name) == 0;
        if (match) {
            AcquireSRWLockExclusive(&w->lock);
            queue_push(w, ww->slot, ww->gen, name);
            ReleaseSRWLockExclusive(&w->lock);
            any = true;
        }
        if (!info->NextEntryOffset) break;
        p += info->NextEntryOffset;
    }
    if (any) win_wake(w);
}

static void win_drain_commands(na_watch *w)
{
    for (;;) {
        AcquireSRWLockExclusive(&w->lock);
        if (w->cmd_head == w->cmd_tail) {
            ReleaseSRWLockExclusive(&w->lock);
            return;
        }
        int kind = w->cmdq[w->cmd_head].kind;
        watch_entry *e = w->cmdq[w->cmd_head].e;
        w->cmd_head = (w->cmd_head + 1) % NA_WATCH_MAX;
        ReleaseSRWLockExclusive(&w->lock);
        if (kind != 2 || !e) continue;
        int slot = (int)(e - w->items);
        win_watch *ww = w->ww[slot];
        if (!ww) continue;
        if (ww->pending) {
            ww->dead = true;
            CancelIoEx(ww->dir, &ww->ov); /* 取消之后会来一条完成，那时再回收 */
        } else {
            win_free_ww(w, ww);
        }
    }
}

static DWORD WINAPI win_thread(LPVOID ud)
{
    na_watch *w = ud;
    for (;;) {
        DWORD bytes = 0;
        ULONG_PTR key = 0;
        LPOVERLAPPED ov = NULL;
        BOOL ok = GetQueuedCompletionStatus(w->iocp, &bytes, &key, &ov, INFINITE);
        if (key == WIN_KEY_STOP) break;
        if (key == WIN_KEY_CMD) {
            win_drain_commands(w);
            continue;
        }
        win_watch *ww = (win_watch *)key;
        if (!ww) continue;
        ww->pending = false;
        if (ww->dead) {
            win_free_ww(w, ww);
            continue;
        }
        if (ok) win_scan(w, ww, bytes);
        else if (GetLastError() == ERROR_NOTIFY_ENUM_DIR) win_notify(w, ww, NULL);
        if (!win_issue(ww)) win_notify(w, ww, NULL);
    }
    return 0;
}

static int be_init(na_watch *w)
{
    WSADATA wsa;
    WSAStartup(MAKEWORD(2, 2), &wsa);
    InitializeSRWLock(&w->lock);
    w->iocp = CreateIoCompletionPort(INVALID_HANDLE_VALUE, NULL, 0, 0);
    if (!w->iocp) return -1;
    SOCKET s = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (s == INVALID_SOCKET) {
        CloseHandle(w->iocp);
        w->iocp = NULL;
        return -1;
    }
    struct sockaddr_in addr;
    memset(&addr, 0, sizeof addr);
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    addr.sin_port = 0;
    if (bind(s, (struct sockaddr *)&addr, sizeof addr) != 0) {
        closesocket(s);
        CloseHandle(w->iocp);
        w->iocp = NULL;
        return -1;
    }
    u_long nonblock = 1;
    ioctlsocket(s, FIONBIO, &nonblock); /* 主循环排空唤醒字节时不能阻塞 */
    int len = sizeof addr;
    getsockname(s, (struct sockaddr *)&addr, &len);
    w->wake_addr = addr;
    w->wake_sock = s;
    w->fd = (intptr_t)s; /* SOCKET 在 x64 上是 64 位句柄，必须按 intptr_t 保存 */
    w->thread = CreateThread(NULL, 0, win_thread, w, 0, NULL);
    if (!w->thread) {
        closesocket(s);
        CloseHandle(w->iocp);
        w->iocp = NULL;
        w->fd = -1;
        return -1;
    }
    w->thread_started = true;
    return 0;
}

static void be_free(na_watch *w)
{
    if (w->thread_started) {
        /* 先让工作线程把挂起的观察都取消并回收，再停线程。 */
        for (int i = 0; i < w->n; i++) be_detach(w, &w->items[i]);
        for (int waited = 0; waited < 500; waited++) {
            bool any = false;
            for (int i = 0; i < NA_WATCH_MAX; i++)
                if (w->ww[i]) { any = true; break; }
            if (!any) break;
            Sleep(5);
        }
        PostQueuedCompletionStatus(w->iocp, 0, WIN_KEY_STOP, NULL);
        WaitForSingleObject(w->thread, 5000);
        CloseHandle(w->thread);
        w->thread_started = false;
    }
    for (int i = 0; i < NA_WATCH_MAX; i++) {
        win_watch *ww = w->ww[i];
        if (!ww) continue;
        if (ww->pending) {
            CancelIoEx(ww->dir, &ww->ov);
            Sleep(20); /* 让取消落地再关句柄 */
        }
        win_free_ww(w, ww);
    }
    if (w->iocp) CloseHandle(w->iocp);
    if (w->fd >= 0) closesocket((SOCKET)(uintptr_t)w->fd);
    w->iocp = NULL;
    w->fd = -1;
}

static int be_attach(na_watch *w, watch_entry *e)
{
    int slot = (int)(e - w->items);
    e->gen = ++w->gen;
    wchar_t path[WATCH_PATH_MAX];
    int need = MultiByteToWideChar(CP_UTF8, 0, e->path, -1, path, WATCH_PATH_MAX);
    if (need <= 0) return -1;
    DWORD attrs = GetFileAttributesW(path);
    bool isdir = attrs != INVALID_FILE_ATTRIBUTES && (attrs & FILE_ATTRIBUTE_DIRECTORY);
    wchar_t dirpath[WATCH_PATH_MAX];
    const wchar_t *name = NULL;
    wcscpy(dirpath, path);
    if (!isdir) {
        /* 明确文件：观察它所在的目录，按文件名过滤 */
        wchar_t *slash = wcsrchr(dirpath, L'\\');
        if (!slash) {
            e->wd = -1;
            return -1;
        }
        *slash = 0;
        name = path + (slash - dirpath) + 1;
    }
    HANDLE dir = CreateFileW(dirpath, FILE_LIST_DIRECTORY,
                             FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, NULL, OPEN_EXISTING,
                             FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OVERLAPPED, NULL);
    if (dir == INVALID_HANDLE_VALUE) {
        e->wd = -1;
        e->next = (int64_t)time(NULL) + NA_WATCH_RETRY / 1000;
        return -1;
    }
    win_watch *ww = calloc(1, sizeof *ww);
    if (!ww) {
        CloseHandle(dir);
        return -1;
    }
    ww->dir = dir;
    ww->slot = slot;
    ww->gen = e->gen;
    if (name) WideCharToMultiByte(CP_UTF8, 0, name, -1, ww->name, sizeof ww->name, NULL, NULL);
    if (!CreateIoCompletionPort(dir, w->iocp, (ULONG_PTR)ww, 0)) {
        CloseHandle(dir);
        free(ww);
        return -1;
    }
    w->ww[slot] = ww;
    if (!win_issue(ww)) {
        win_free_ww(w, ww);
        e->wd = -1;
        e->next = (int64_t)time(NULL) + NA_WATCH_RETRY / 1000;
        return -1;
    }
    e->wd = 1;
    e->next = 0;
    return 1;
}

static void be_detach(na_watch *w, watch_entry *e)
{
    if (e->wd < 0) return;
    AcquireSRWLockExclusive(&w->lock);
    w->cmdq[w->cmd_tail].kind = 2;
    w->cmdq[w->cmd_tail].e = e;
    w->cmd_tail = (w->cmd_tail + 1) % NA_WATCH_MAX;
    ReleaseSRWLockExclusive(&w->lock);
    PostQueuedCompletionStatus(w->iocp, 0, WIN_KEY_CMD, NULL);
    e->wd = -1;
}

static void be_ready(na_watch *w, int64_t now)
{
    if (w->fd < 0) return;
    char b[256];
    while (recv((SOCKET)(uintptr_t)w->fd, b, sizeof b, 0) > 0) { }
    int64_t due = now + NA_WATCH_DEBOUNCE;
    AcquireSRWLockExclusive(&w->lock);
    bool overflow = w->overflow;
    if (overflow) w->pend_head = w->pend_tail = 0;
    w->overflow = false;
    pending_ev ev;
    while (queue_pop(w, &ev)) {
        watch_entry *e = watch_by_slot(w, ev.slot);
        if (!e || e->gen != ev.gen) continue;
        char full[WATCH_PATH_MAX];
        join_path(full, sizeof full, e->path, ev.name);
        memcpy(e->last, full, sizeof e->last);
        e->count++;
        e->due = due;
    }
    ReleaseSRWLockExclusive(&w->lock);
    if (overflow) {
        for (int i = 0; i < w->n; i++) {
            w->items[i].resync = true;
            w->items[i].due = due;
        }
    }
}
#endif /* _WIN32 */

/* 其他平台：明确不支持，不虚报能力（hello 里的 file_watch 也就是 false）。 */
#if !defined(__linux__) && !defined(__APPLE__) && !defined(_WIN32)
static int  be_init(na_watch *w) { (void)w; return -1; }
static void be_free(na_watch *w) { (void)w; }
static int  be_attach(na_watch *w, watch_entry *e) { (void)w; (void)e; return -1; }
static void be_detach(na_watch *w, watch_entry *e) { (void)w; (void)e; }
static void be_ready(na_watch *w, int64_t now) { (void)w; (void)now; }
#endif

/* ---------- 公共接口 ---------- */
na_watch *na_watch_new(void)
{
    na_watch *w = calloc(1, sizeof *w);
    if (!w) return NULL;
    w->fd = -1;
    for (int i = 0; i < NA_WATCH_MAX; i++) w->items[i].wd = -1;
#if defined(__APPLE__)
    w->kq = -1;
    w->cmd[0] = w->cmd[1] = w->wake[0] = w->wake[1] = -1;
#elif defined(_WIN32)
    w->iocp = NULL;
    w->wake_sock = INVALID_SOCKET;
#endif
    if (be_init(w) != 0) {
        /* 初始化失败：不虚报支持，主循环也就不会等这个 fd */
        w->fd = -1;
    }
    return w;
}

void na_watch_free(na_watch *w)
{
    if (!w) return;
    be_free(w);
    free(w);
}

bool na_watch_supported(const na_watch *w) { return w && w->fd >= 0; }
intptr_t na_watch_fd(const na_watch *w) { return w ? w->fd : -1; }

void na_watch_sync(na_watch *w, const cJSON *list, bool resync, int64_t now)
{
    if (!w) return;
    bool keep[NA_WATCH_MAX];
    memset(keep, 0, sizeof keep);
    const cJSON *it = NULL;
    cJSON_ArrayForEach(it, list) {
        const char *key = sget(it, "key"), *path = sget(it, "path");
        if (!key || !path || !*key || !*path) continue;
        if (strlen(key) >= WATCH_KEY_MAX || strlen(path) >= WATCH_PATH_MAX) continue;
        int slot = -1;
        for (int i = 0; i < w->n; i++)
            if (w->items[i].used && !strcmp(w->items[i].key, key)) { slot = i; break; }
        if (slot < 0) {
            /* 槽位身份稳定：新登记用第一个空槽，不搬动已经在用的条目——
             * 后台对象（Windows 的 win_watch、macOS 的 kevent.udata）按槽位下标认人，
             * 搬动会让保留下来的订阅指向旧下标，之后的变化全被丢掉。 */
            for (int i = 0; i < NA_WATCH_MAX; i++) {
                if (!w->items[i].used) { slot = i; break; }
            }
            if (slot < 0) continue; /* 超过上限的登记明确不接 */
            if (slot >= w->n) w->n = slot + 1;
            memset(&w->items[slot], 0, sizeof w->items[slot]);
            w->items[slot].wd = -1;
            w->items[slot].used = true;
            snprintf(w->items[slot].key, sizeof w->items[slot].key, "%s", key);
        }
        watch_entry *e = &w->items[slot];
        keep[slot] = true;
        if (strcmp(e->path, path)) {
            be_detach(w, e);
            snprintf(e->path, sizeof e->path, "%s", path);
            e->last[0] = 0;
        }
        if (e->wd < 0) {
            e->wd = be_attach(w, e);
            if (e->wd < 0) e->next = now + NA_WATCH_RETRY;
        }
        if (resync) {
            e->resync = true;
            if (!e->due) e->due = now;
        }
    }
    /* 摘除只清自己那个槽位，不移动别人：槽位身份必须稳定 */
    for (int i = 0; i < w->n; i++) {
        if (!w->items[i].used || keep[i]) continue;
        be_detach(w, &w->items[i]);
        memset(&w->items[i], 0, sizeof w->items[i]);
        w->items[i].wd = -1;
    }
    while (w->n > 0 && !w->items[w->n - 1].used) w->n--;
}

void na_watch_ready(na_watch *w, int64_t now)
{
    if (!w || w->fd < 0) return;
    be_ready(w, now);
}

void na_watch_tick(na_watch *w, int64_t now, na_watch_emit emit, void *ud)
{
    if (!w || !emit) return;
    for (int i = 0; i < w->n; i++) {
        watch_entry *e = &w->items[i];
        if (!e->used) continue;
        if (e->wd < 0 && e->next && now >= e->next) {
            e->wd = be_attach(w, e);
            e->next = e->wd < 0 ? now + NA_WATCH_RETRY : 0;
        }
        if (!e->due || e->due > now) continue;
        emit(ud, e->key, e->last[0] ? e->last : e->path, e->count, e->resync);
        e->count = 0;
        e->resync = false;
        e->due = 0;
    }
}

int64_t na_watch_next(const na_watch *w)
{
    if (!w) return -1;
    int64_t next = -1;
    for (int i = 0; i < w->n; i++) {
        if (!w->items[i].used) continue;
        int64_t due = w->items[i].due;
        if (due && (next < 0 || due < next)) next = due;
    }
    return next;
}
