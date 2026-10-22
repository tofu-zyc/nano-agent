/* update.h —— 内核自我更新：网关推来新版本的程序，换掉自己；新版本起不来就退回旧版本 */
#ifndef NA_UPDATE_H
#define NA_UPDATE_H

#include "cJSON.h"
#include <stdbool.h>
#include <stddef.h>

#define NA_EXIT_RESTART   75      /* 请外面（Windows 的 run-kernel.ps1、systemd）立即重新拉起 */
#define UPDATE_CONFIRM_MS 90000   /* 更新后这么久还没被网关接受，就退回旧版本 */
#define UPDATE_MAX_TRIES  3       /* 新版本最多启动几次 */

/* 进程启动时最先调用：记下内核所在的目录（之后自己的文件会被改名，/proc/self/exe 就不准了） */
void update_init(void);
/* 启动时调用。上次的更新还没确认成功：记一次启动，*pending = true（调用方限时等网关接受）；
 * 启动次数超了就退回旧版本并返回 true，调用方应该马上重启自己。 */
bool update_on_start(bool *pending);
/* 网关接受连接后调用：这次更新成功了 */
void update_confirm(void);
/* 退回旧版本。成功返回 0，调用方随后重启自己 */
int update_rollback(const char *why);
/* 上次失败、已经退回的版本（hello 里告诉网关，别再自动推同一个版本）；没有返回 NULL。malloc */
char *update_failed_version(void);
/* 处理 update 消息：把新程序写到内核旁边，再换掉旧的（旧的留成 .old）。
 * exec 是执行器的路径，必须和内核在同一个目录（安装脚本装的布局）。 */
int update_apply(const cJSON *m, const char *exec, char *err, size_t errlen);
/* 重启自己：POSIX 上用同样的参数 exec 目录里的 nano-kernel（成功不返回）；
 * Windows 上直接返回，调用方以 NA_EXIT_RESTART 退出 */
void update_reexec(char **argv);

#endif
