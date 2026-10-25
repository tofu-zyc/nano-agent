/* pipe.h —— 协议 v10：MCP 服务的 stdio 透传
 *
 * 网关让内核起一个子进程（pipe_open），往它的 stdin 写一行（pipe_write），
 * 内核把它 stdout 的每一行用 pipe_data 送回网关，进程退出时发 pipe_closed。
 * 这样 MCP 服务（比如 computer use）可以挂在装了桌面环境的机器上跑。
 */
#ifndef NA_PIPE_H
#define NA_PIPE_H

#include "plat.h"

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* 一条管道。fd_out 是可读的 stdout，fd_in 是可写的 stdin（都是非阻塞）。 */
typedef struct na_pipe {
    char   *id;
    int     fd_in, fd_out;
    int     pid;
    char   *buf;          /* 还没凑成完整一行的尾巴 */
    size_t  len, cap;
    bool    closing;      /* 网关说关了：stdin 已关，等它退出 */
    int64_t close_at;
    bool    reported;     /* 已经发过 pipe_closed */
    bool    reaped;       /* waitpid 已经收过 */
    int     code;
    struct na_pipe *next;
} na_pipe_t;

/* 等着发给网关的东西：一行 stdout，或者一次退出。 */
typedef struct {
    char   *id;
    char   *data;   /* pipe_data 的一行；退出的那条是 NULL */
    int     exit;   /* >= 0 表示 pipe_closed（退出码） */
    char   *detail; /* 退出的原因 */
} na_pipe_msg;

typedef struct {
    na_pipe_t    *list;
    na_pipe_msg  *q;
    size_t        qn, qcap, qhead;
    int64_t       close_grace_ms; /* 关了 stdin 多久还没退出就杀 */
} na_pipes;

void pipes_init(na_pipes *ps);
/* 起一个子进程；失败返回 -1，原因写进 err。env 是 k,v 交替、NULL 结尾。 */
int  pipes_open(na_pipes *ps, const char *id, char *const argv[], const char *const env[], const char *cwd,
                char *err, size_t errlen);
int  pipes_write(na_pipes *ps, const char *id, const char *data);
void pipes_close(na_pipes *ps, const char *id);
/* 把管道的读端填进 poll（POSIX）；Windows 上是 0 个。refs 记每项属于哪条管道。 */
size_t pipes_pollfds(na_pipes *ps, na_pollfd *pfds, na_pipe_t **refs, size_t max);
void pipes_poll(na_pipes *ps, const na_pollfd *pfds, na_pipe_t **refs, size_t n, int64_t now);
/* 取走一条要发的东西；取空了返回 false。调用方 free 掉 id / data / detail。 */
bool pipes_take(na_pipes *ps, na_pipe_msg *out);
void pipes_shutdown(na_pipes *ps);

#endif
