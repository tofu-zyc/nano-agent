/* runner.h —— 子进程任务：启动执行器，喂请求，收结果
 *
 * 每次工具调用启动一个执行器进程（本机是 nano-exec，以后远程是 ssh … nano-exec）。
 * 请求写进它的 stdin 后 stdin 保持打开；终止时先关 stdin、发 SIGTERM，让执行器
 * 自己清理它启动的命令，过一会儿还没退出再 SIGKILL。
 */
#ifndef NA_RUNNER_H
#define NA_RUNNER_H

#include "util.h"

#include <poll.h>
#include <stdbool.h>
#include <sys/types.h>

typedef struct job {
    char   *sid, *id, *name;
    pid_t   pid;
    int     fd_in, fd_out, fd_err;
    char   *in;                    /* 写给子进程 stdin 的请求 */
    size_t  in_len, in_off;
    buf_t   out, err;
    size_t  out_total, err_total;  /* 实际产生的字节数，超出上限的部分丢弃 */
    bool    exited, killed;
    int     status;
    int64_t exit_at, term_at;
    struct job *next;
} job_t;

typedef struct {
    job_t *jobs;
    size_t out_cap;   /* stdout 最多保留多少字节 */
} runner_t;

/* poll 集成：runner_pollfds 填 pollfd，refs 记录每一项属于哪个任务的哪个管道 */
typedef struct {
    job_t *job;
    int    which;   /* 0 = stdin, 1 = stdout, 2 = stderr */
} pollref_t;

/* 启动 argv，把 input（接管所有权）写进它的 stdin。失败返回 -1，原因写进 err。 */
int    runner_start(runner_t *r, char *const argv[], const char *sid, const char *id, const char *name,
                    char *input, char *err, size_t errlen);
void   runner_kill(runner_t *r, const char *sid, const char *id);
void   runner_kill_all(runner_t *r);
void   runner_reap(runner_t *r, int64_t now);
void   runner_tick(runner_t *r, int64_t now);   /* SIGTERM 之后迟迟不退出的，改用 SIGKILL */
size_t runner_pollfds(runner_t *r, struct pollfd *pfds, pollref_t *refs, size_t max);
void   runner_on_poll(runner_t *r, const struct pollfd *pfds, const pollref_t *refs, size_t n);
job_t *runner_take_finished(runner_t *r, int64_t now);
size_t runner_count(const runner_t *r);

/* 解析执行器的输出 {"ok":…,"content":…}；执行器异常时根据退出状态和 stderr 生成说明 */
char  *job_result(const job_t *j, bool *ok);
void   job_free(job_t *j);

#endif
