/* pipe.h —— 协议 v10：MCP 服务的 stdio 透传
 *
 * 网关让内核起一个子进程（pipe_open），往它的 stdin 写一行（pipe_write），
 * 内核把它 stdout 的每一行用 pipe_data 送回网关，进程退出时发 pipe_closed。
 *
 * POSIX 走 poll（pipe.c），Windows 用读线程 + 唤醒 socket（pipe_win.c）。
 */
#ifndef NA_PIPE_H
#define NA_PIPE_H

#include "plat.h"

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct na_pipe {
    char   *id;
    /* POSIX */
    int     fd_in, fd_out;
    int     pid;
    /* Windows：stdin 由写线程独占（主循环不能阻塞在 WriteFile 上），wev 叫醒它 */
    void   *hproc, *hjob, *h_in, *h_out, *wev;
    bool    killed;
    bool    in_eof;       /* 写完缓冲就关 stdin（pipe_close / 进程退出 / 网关断开） */
    /* 两边共用 */
    char   *buf;          /* stdout 还没凑成完整一行的尾巴 */
    size_t  len, cap;
    char   *wbuf;         /* 还没写进 stdin 的（写满了先存着，能写时再写） */
    size_t  wlen, woff;
    bool    input_dead;   /* stdin 已经坏了（对端关了） */
    bool    closing;
    int64_t close_at;
    bool    reported, reaped;
    int     live;          /* Windows：还有几个线程在用这条管道 */
    int     code;
    struct na_pipe *next;
} na_pipe_t;

/* poll 里的一项：属于哪条管道的哪一头（0 = stdin 可写，1 = stdout 可读） */
typedef struct {
    na_pipe_t *p;
    int        which;
} na_piperef;

/* 等着发给网关的东西：一行 stdout，或者一次退出。 */
typedef struct {
    char *id;
    char *data;   /* pipe_data 的一行；退出的那条是 NULL（靠它区分，不靠 exit） */
    int   exit;   /* pipe_closed 的退出码，-1 表示没有这条管道、写不进去 */
    char *detail; /* 退出的原因 */
} na_pipe_msg;

typedef struct {
    na_pipe_t   *list;
    na_pipe_msg *q;
    size_t       qn, qcap, qhead;
    int64_t      close_grace_ms;
    void        *lock;
    na_wake     *wake;
} na_pipes;

void pipes_init(na_pipes *ps);
void pipes_set_wake(na_pipes *ps, na_wake *w);
int  pipes_open(na_pipes *ps, const char *id, char *const argv[], const char *const env[], const char *cwd,
                char *err, size_t errlen);
int  pipes_write(na_pipes *ps, const char *id, const char *data);
void pipes_close(na_pipes *ps, const char *id);
size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_piperef *refs, size_t max);
void pipes_poll(na_pipes *ps, const na_pollfd *pfds, const na_piperef *refs, size_t n, int64_t now);
bool pipes_take(na_pipes *ps, na_pipe_msg *out);
/* 网关断开了：杀掉所有进程、丢掉没发的消息。条目先不释放（主循环这一轮的 poll 引用还指着它们），
   下一次 pipes_poll 收尾时释放。 */
void pipes_reset(na_pipes *ps);
/* 内核退出：杀掉所有进程并释放全部条目。之后不能再调 pipes_poll。 */
void pipes_shutdown(na_pipes *ps);
/* 表里还有几条管道（包括已经退出、等着释放的） */
size_t pipes_count(const na_pipes *ps);

/* 内部（pipe.c 和 pipe_win.c 共用） */
na_pipe_t *find_pipe(na_pipes *ps, const char *id);
void pipes_lock(na_pipes *ps);
void pipes_unlock(na_pipes *ps);
void pipes_queue_line(na_pipes *ps, const char *id, const char *line, size_t n);
void pipes_queue_closed(na_pipes *ps, const char *id, int code, const char *detail);
void free_pipe(na_pipe_t *p);
void pipes_clear_queue(na_pipes *ps);

#endif
