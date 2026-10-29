/* watch.h —— 有界的文件观察登记表（设计文档 §6.6）
 *
 * 只登记明确文件或非递归目录。Linux 用 inotify（主循环直接轮询 inotify fd）；
 * macOS 用 kqueue + 工作线程 + 管道唤醒；Windows 用一个 IOCP 工作线程 +
 * ReadDirectoryChangesW，主循环轮询回环 UDP 套接字。只有初始化成功时
 * na_watch_supported() 才返回 true，内核据此决定 hello 里宣不宣告 file_watch。
 * 事件只表示“至少观察到一次变化”，不是每次写入的完整审计：队列溢出、掉线或重启
 * 之后用 resync 说明缺口，让处理器按限定范围重新比较状态。 */
#ifndef NA_WATCH_H
#define NA_WATCH_H

#include "cJSON.h"

#include <stdbool.h>
#include <stdint.h>

#define NA_WATCH_MAX      64      /* 同时登记的路径数上限 */
#define NA_WATCH_DEBOUNCE 500     /* 同一路径短时间内的多次变化合成一条 */
#define NA_WATCH_RETRY    5000    /* 观察不到的路径隔多久再试一次 */

typedef struct na_watch na_watch;

/* 发出一条观察结果：count 是这段时间看到的变化次数，resync 表示这里有缺口。 */
typedef void (*na_watch_emit)(void *ud, const char *key, const char *path, int count, bool resync);

na_watch *na_watch_new(void);
void      na_watch_free(na_watch *w);
bool      na_watch_supported(const na_watch *w);
/* 主循环轮询的句柄：POSIX 是 fd，Windows 是套接字（64 位，所以用 intptr_t，不能用 int）。 */
intptr_t  na_watch_fd(const na_watch *w);
/* 用网关给的完整集合替换登记。resync 为 true 时现有登记各报一次缺口（重新连上之后用）。 */
void      na_watch_sync(na_watch *w, const cJSON *list, bool resync, int64_t now);
void      na_watch_ready(na_watch *w, int64_t now);              /* poll 说可读：读走内核队列 */
void      na_watch_tick(na_watch *w, int64_t now, na_watch_emit emit, void *ud);
int64_t   na_watch_next(const na_watch *w);                      /* 下一条事件最早什么时候发；没有返回 -1 */

#endif
