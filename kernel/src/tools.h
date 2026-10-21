/* tools.h —— 本地工具：从 tools/ 目录加载，以子进程方式执行
 *
 * 每个工具一个子目录：
 *   tool.json  名字、描述、input_schema，以及 timeout / allow / approval / command
 *   run        默认的可执行文件：stdin 读参数 JSON，stdout 写结果，退出码非 0 表示失败
 */
#ifndef NA_TOOLS_H
#define NA_TOOLS_H

#include "cJSON.h"
#include "core.h"
#include "util.h"

#include <poll.h>
#include <sys/types.h>

typedef struct {
    char  *name;
    char  *dir;
    char **argv;          /* 以 NULL 结尾，在工具目录下执行 */
    cJSON *desc;          /* {"name","description","input_schema"}，原样报给网关 */
    na_tool_policy pol;
} tool_t;

typedef struct {
    tool_t *v;
    size_t  n;
} toolset_t;

int  toolset_load(toolset_t *ts, const char *dir);   /* 返回加载到的工具数；目录打不开返回 -1 */
void toolset_free(toolset_t *ts);
const tool_t *toolset_find(const toolset_t *ts, const char *name);

typedef struct job {
    char   *sid, *id, *name;
    pid_t   pid;
    int     fd_in, fd_out, fd_err;
    char   *in;                    /* 写给子进程 stdin 的参数 */
    size_t  in_len, in_off;
    buf_t   out, err;
    size_t  out_total, err_total;  /* 实际产生的字节数，超出上限的部分丢弃 */
    bool    exited, killed;
    int     status;
    int64_t exit_at;
    struct job *next;
} job_t;

typedef struct {
    job_t *jobs;
    size_t max_output;
} runner_t;

/* poll 集成：runner_pollfds 填 pollfd，refs 记录每一项属于哪个任务的哪个管道 */
typedef struct {
    job_t *job;
    int    which;   /* 0 = stdin, 1 = stdout, 2 = stderr */
} pollref_t;

int    runner_exec(runner_t *r, const tool_t *t, const na_cmd *cmd, char *err, size_t errlen);
void   runner_kill(runner_t *r, const char *sid, const char *id);
void   runner_kill_all(runner_t *r);
void   runner_reap(runner_t *r, int64_t now);
size_t runner_pollfds(runner_t *r, struct pollfd *pfds, pollref_t *refs, size_t max);
void   runner_on_poll(runner_t *r, const struct pollfd *pfds, const pollref_t *refs, size_t n);
job_t *runner_take_finished(runner_t *r, int64_t now);
size_t runner_count(const runner_t *r);

char  *job_result(const job_t *j, size_t max_output, bool *ok);
void   job_free(job_t *j);

#endif
