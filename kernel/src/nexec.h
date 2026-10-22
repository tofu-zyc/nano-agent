/* nexec.h —— nano-exec：在沙箱里执行一次内置工具
 *
 * 用法：stdin 写一行请求 JSON，stdout 回一行结果 JSON，然后退出。
 *   请求：{"tool":"Read","args":{...},"dirs":["/work"],"write":true,"ro":[],"rw":[],
 *          "tmp_key":"cli_local","max_output":65536,"watch_stdin":true}
 *   结果：{"ok":true,"content":"..."}
 * watch_stdin 为 true 时，读完请求后 stdin 被关闭就表示取消：执行器杀掉正在跑的命令并退出。
 * 内核在本机直接启动它，远程主机上则通过 ssh 启动，行为完全一样。
 */
#ifndef NA_NEXEC_H
#define NA_NEXEC_H

#include "cJSON.h"
#include "util.h"

#include <stdbool.h>
#include <stddef.h>

#define READ_DEFAULT_LIMIT 2000    /* Read 默认最多读多少行 */
#define READ_LINE_MAX      2000    /* 单行最多显示多少字节 */
#define SHELL_DEFAULT_TIMEOUT 120  /* 秒 */
#define SHELL_MAX_TIMEOUT     600
#define GREP_TIMEOUT           60

typedef struct {
    char **dirs;  size_t ndirs;   /* 工作目录（已解析符号链接），第一个是默认目录 */
    bool   write;                 /* 工作目录是否可写（只读模式下为 false） */
    bool   home_only;             /* 没有挂工作目录：相对路径和 Shell 的当前目录用 HOME */
    char **ro;    size_t nro;     /* 额外的只读路径 */
    char **rw;    size_t nrw;     /* 额外的读写路径 */
    char  *home;
    char  *tmp;                   /* 这个会话的临时目录，TMPDIR 指向它 */
    char  *cache;                 /* 各会话共用的缓存目录，XDG_CACHE_HOME 指向它 */
    size_t max_output;
    bool   watch_stdin;
    char **env;                   /* 子进程的环境变量，以 NULL 结尾 */
} xctx;

/* 工具实现：成功返回 true；无论成败，给模型看的内容都写进 out */
bool tool_read(const xctx *x, const cJSON *args, buf_t *out);
bool tool_write(const xctx *x, const cJSON *args, buf_t *out);
bool tool_edit(const xctx *x, const cJSON *args, buf_t *out);
bool tool_grep(const xctx *x, const cJSON *args, buf_t *out);
bool tool_shell(const xctx *x, const cJSON *args, buf_t *out);

/* ---- 路径（nexec_fs.c） ---- */

/* 把模型给的路径变成绝对、解析过符号链接的真实路径。文件本身可以不存在（Write 新文件），
 * 这时解析到最近的已存在的上级目录，再接上剩下的部分。失败返回 NULL，原因写进 out。 */
char *path_resolve(const xctx *x, const char *p, buf_t *out);
bool  path_inside(const char *path, const char *dir);
/* 检查能否读 / 写这个真实路径；不能时把原因写进 out */
bool  path_can_read(const xctx *x, const char *real, buf_t *out);
bool  path_can_write(const xctx *x, const char *real, buf_t *out);
char *expand_home(const char *home, const char *p);

/* ---- 子进程（nexec_proc.c） ---- */

typedef struct {
    int  status;       /* waitpid 的状态 */
    bool timed_out;
} proc_status;

/* 在 cwd 下执行 argv（stdin 是 /dev/null，stdout 和 stderr 合在一起），最多 timeout_s 秒。
 * 输出保留开头和结尾、超出部分省略中间，结果写进 out。返回 0；无法启动返回 -1。
 * 期间被取消（stdin 关闭或收到 SIGTERM）时杀掉命令，执行器直接退出。 */
int   run_proc(const xctx *x, char *const argv[], const char *cwd, int timeout_s,
               buf_t *out, proc_status *st);
char *find_in_path(const xctx *x, const char *name);   /* 找不到返回 NULL */

#endif
