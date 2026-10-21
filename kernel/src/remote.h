/* remote.h —— 通过 SSH 在服务器上执行工具（服务器上不跑内核）
 *
 * 每台服务器要先准备好执行器，在后台进行，不阻塞主循环：
 *   1. 检查：服务器上有没有这个版本的 nano-exec、能不能跑（--probe）；
 *   2. 上传：没有的话，架构相同就把本机的传上去（有静态编译的 nano-exec-static 就传它）；
 *   3. 现编：传上去跑不起来（比如服务器的 glibc 太老）而服务器有 cc，就把源码传过去编译。
 * 执行器放在服务器的 ~/.cache/nano-agent/nano-exec-<版本>，也可以在配置里指定现成的路径。
 *
 * 每次工具调用就是 ssh 目标 执行器，请求和结果和本机一样走 stdin / stdout；SSH 连接复用
 * （ControlMaster），关掉 stdin 就是取消。准备好的服务器每隔几分钟再检查一次，调用时连不上
 * 也会重新准备；状态变了告诉网关。
 */
#ifndef NA_REMOTE_H
#define NA_REMOTE_H

#include "cJSON.h"
#include "conf.h"
#include "runner.h"

#include <stdbool.h>
#include <stdint.h>

/* runner 里的任务类型 */
enum { JOB_TOOL = 0, JOB_REMOTE_TOOL, JOB_PREP, JOB_LS };

typedef enum { RM_IDLE, RM_PROBE, RM_UPLOAD, RM_BUILD, RM_READY, RM_FAILED } rm_state;

typedef struct {
    const kremote *conf;
    rm_state state;
    int      landlock;     /* 服务器上的沙箱版本，0 表示没有 */
    char    *detail;       /* 失败的原因 */
    char    *arch, *cc;    /* 检查时得到的服务器架构和编译器（没有编译器时为空） */
    int64_t  next_at;      /* 下一次检查的时刻 */
    bool     busy;         /* 有准备任务在跑 */
} remote_t;

typedef struct {
    const kconf *conf;
    remote_t *list;
    size_t    n;
    char     *ver;         /* 执行器的版本：本机二进制的指纹，服务器上的文件名带着它 */
    char     *ctl_dir;     /* ssh ControlPath 所在的目录（只有自己能进） */
    char     *upload_bin;  /* 上传用的执行器 */
    char     *src_tar;     /* 现编用的源码包，没有时为空 */
    char      arch[80];    /* 本机架构 */
    bool      changed;     /* 有服务器的状态变了，要告诉网关 */
} remotes_t;

int       remotes_init(remotes_t *rs, const kconf *conf, char *err, size_t errlen);
void      remotes_free(remotes_t *rs);
remote_t *remotes_find(remotes_t *rs, const char *name);
int       remotes_index(const remotes_t *rs, const remote_t *rm);

/* 该检查的检查（启动时、失败后过一会儿、准备好之后定期） */
void      remotes_tick(remotes_t *rs, runner_t *r, int64_t now);
/* 准备服务器的任务结束了 */
void      remotes_on_job(remotes_t *rs, runner_t *r, const job_t *j, int64_t now);
/* 调用时连不上服务器：标记失败，过一会儿重新准备 */
void      remote_failed(remotes_t *rs, remote_t *rm, const char *why, int64_t now);

/* 在服务器上运行执行器的 ssh 参数（args 是给执行器的参数，可以为 NULL）。用 argv_free 释放 */
char    **remote_exec_argv(const remotes_t *rs, const remote_t *rm, const char *args);
/* 上传一个技能目录，完成后输出工具结果 JSON；remote_dir 是 ~/ 开头的缓存路径 */
char    **remote_skill_argv(const remotes_t *rs, const remote_t *rm, const char *dir,
                           const char *remote_dir, const char *result);
void      argv_free(char **argv);
char     *shell_quote(const char *s);

/* [{"name","ok","landlock","detail"}]，hello 和 remotes 消息里用 */
cJSON    *remotes_json(const remotes_t *rs);

#endif
