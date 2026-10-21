/* sandbox.h —— 执行器的沙箱
 *
 * 三层限制，套上之后对当前进程和之后启动的所有子进程都生效，且不能撤销：
 *   - Landlock：只能访问规则里列出的目录和文件，其余一律拒绝；不能给沙箱外的进程发信号；
 *   - seccomp：不能创建 unix socket（否则可以通过 D-Bus、systemd、docker 等在沙箱外执行命令），
 *     不能用 io_uring（可以绕过 seccomp），不能用 32 位兼容的系统调用；
 *   - no_new_privs：setuid 程序不再提权。
 */
#ifndef NA_SANDBOX_H
#define NA_SANDBOX_H

#include <stddef.h>

typedef struct {
    const char *const *rw; size_t nrw;   /* 可读、可写、可执行 */
    const char *const *ro; size_t nro;   /* 只读、可执行 */
} sb_rules;

/* 本机支持的 Landlock ABI 版本，0 表示不可用 */
int sb_landlock_abi(void);

/* 套上沙箱。不存在的路径直接跳过。Landlock 不可用时只套 seccomp 并返回 1；
 * 成功返回 0；失败返回 -1，原因写进 err。 */
int sb_apply(const sb_rules *r, char *err, size_t errlen);

#endif
