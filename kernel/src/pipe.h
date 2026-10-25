/* pipe.h —— 协议 v10：MCP 服务的 stdio 透传
 *
 * 网关让内核起一个子进程（pipe_open），往它的 stdin 写一行（pipe_write），
 * 内核把它 stdout 的每一行用 pipe_data 送回网关，进程退出时发 pipe_closed。
 * 这样 MCP 服务（比如 computer use）可以挂在装了桌面环境的机器上跑。
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
    /* Windows */
    void   *hproc, *hjob, *h_in, *h_out;
    bool    killed;
    /* 两边共用：还没凑成完整一行的尾巴 */
    char   *buf;
    size_t  len, cap;
    bool    closing;
    int64_t close_at;
    bool    reported, reaped;
    int     code;
    struct na_pipe *next;
} na_pipe_t;

/* 等着发给网关的东西：一行 stdout，或者一次退出。 */
typedef struct {
    char *id;
    char *data;   /* pipe_data 的一行；退出的那条是 NULL */
    int   exit;   /* >= 0 表示 pipe_closed（退出码） */
    char *detail; /* 退出的原因 */
} na_pipe_msg;

typedef struct {
    na_pipe_t   *list;
    na_pipe_msg *q;
    size_t       qn, qcap, qhead;
    int64_t      close_grace_ms;
    void        *lock;   /* Windows：CRITICAL_SECTION*；POSIX 不用 */
    na_wake     *wake;   /* Windows：读线程用它叫醒主循环 */
} na_pipes;

void pipes_init(na_pipes *ps);
void pipes_set_wake(na_pipes *ps, na_wake *w);
/* 起一个子进程；失败返回 -1，原因写进 err。env 是 k,v 交替、NULL 结尾。 */
int  pipes_open(na_pipes *ps, const char *id, char *const argv[], const char *const env[], const char *cwd,
                char *err, size_t errlen);
int  pipes_write(na_pipes *ps, const char *id, const char *data);
void pipes_close(na_pipes *ps, const char *id);
size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_pipe_t **refs, size_t max);
void pipes_poll(na_pipes *ps, const na_pollfd *pfds, na_pipe_t **refs, size_t n, int64_t now);
/* 取走一条要发的东西；取空了返回 false。调用方 free 掉 id / data / detail。 */
bool pipes_take(na_pipes *ps, na_pipe_msg *out);
void pipes_shutdown(na_pipes *ps);

/* 内部（pipe.c 和 pipe_win.c 共用） */
na_pipe_t *find_pipe(na_pipes *ps, const char *id);
void pipes_lock(na_pipes *ps);
void pipes_unlock(na_pipes *ps);
void pipes_queue_line(na_pipes *ps, const char *id, const char *line, size_t n);
void pipes_queue_closed(na_pipes *ps, const char *id, int code, const char *detail);

#endif
