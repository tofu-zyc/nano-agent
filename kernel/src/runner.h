/* runner.h —— 子进程任务：启动执行器，喂请求，收结果
 *
 * 每次工具调用启动一个执行器进程（本机是 nano-exec，远程是 ssh … nano-exec）。
 * 请求写进它的 stdin 后 stdin 保持打开；终止时先关 stdin、发 SIGTERM，让执行器
 * 自己清理它启动的命令，过一会儿还没退出再 SIGKILL。
 * POSIX 上这些管道都在内核的 poll 里；Windows 上用两个读线程，读到数据后唤醒 poll。
 */
#ifndef NA_RUNNER_H
#define NA_RUNNER_H

#include "plat.h"
#include "util.h"

#include <stdbool.h>
#include <sys/types.h>

typedef struct job {
    char   *sid, *id, *name;
    char   *run_id;                /* 完整调用身份：哪个 run、哪一轮（宿主填，job_free 释放） */
    int     turn_id;
    char   *res_keys, *res_xkeys;  /* 这个任务占着的资源键：运行期间挡住别的冲突调用 */
    char   *sandbox;               /* 启动时的沙箱模式快照（auto / on / off） */
    int     kind, tag;             /* 调用方自己定义的任务类型和附加信息 */
    bool    close_in;              /* 输入写完就关掉 stdin（上传文件时，对方要读到 EOF） */
    pid_t   pid;
    int     fd_in, fd_out, fd_err;
    char   *in;                    /* 写给子进程 stdin 的请求 */
    size_t  in_len, in_off;
    buf_t   out, err;
    size_t  out_total, err_total;  /* 实际产生的字节数，超出上限的部分丢弃 */
    bool    exited, killed;
    int     status;                /* POSIX wait 状态；Windows 上不用 */
    int     exit_code, term_signal;
    int64_t exit_at, term_at;
#if NA_WINDOWS
    void   *hproc, *hjob, *h_in, *h_out, *h_err;
    void   *th_out, *th_err, *rarg_out, *rarg_err;
    volatile long out_done, err_done;
#endif
    struct job *next;
} job_t;

typedef struct {
    job_t  *jobs;
    size_t  out_cap;   /* stdout 最多保留多少字节 */
    na_wake *wake;     /* Windows 读线程用它唤醒主循环 */
} runner_t;

/* poll 集成：runner_pollfds 填 pollfd，refs 记录每一项属于哪个任务的哪个管道 */
typedef struct {
    job_t *job;
    int    which;   /* 0 = stdin, 1 = stdout, 2 = stderr */
} pollref_t;

/* 启动 argv，把 input（接管所有权）写进它的 stdin。失败返回 -1，原因写进 err。 */
int    runner_start(runner_t *r, char *const argv[], const char *sid, const char *id, const char *name,
                    char *input, char *err, size_t errlen);
/* 同上，input 可以含任意字节（长度 in_len）；成功返回任务，调用方可以再设 kind / tag / close_in */
job_t *runner_start_ex(runner_t *r, char *const argv[], const char *sid, const char *id, const char *name,
                       char *input, size_t in_len, char *err, size_t errlen);
void   runner_kill(runner_t *r, const char *sid, const char *id);
void   runner_kill_all(runner_t *r);
void   runner_reap(runner_t *r, int64_t now);
void   runner_tick(runner_t *r, int64_t now);   /* SIGTERM 之后迟迟不退出的，改用 SIGKILL */
size_t runner_pollfds(runner_t *r, na_pollfd *pfds, pollref_t *refs, size_t max);
void   runner_on_poll(runner_t *r, const na_pollfd *pfds, const pollref_t *refs, size_t n);
job_t *runner_take_finished(runner_t *r, int64_t now);
size_t runner_count(const runner_t *r);
/* 内核退出：停掉所有任务，最多等 grace_ms，然后强制清理 */
void   runner_shutdown(runner_t *r, int grace_ms);

/* 解析执行器的输出 {"ok":…,"content":…}；执行器异常时根据退出状态和 stderr 生成说明 */
char  *job_result(const job_t *j, bool *ok);
void   job_free(job_t *j);

#endif
