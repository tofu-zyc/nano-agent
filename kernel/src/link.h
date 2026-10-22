/* link.h —— 到网关的连接：一行一条 JSON（NDJSON），非阻塞读写 */
#ifndef NA_LINK_H
#define NA_LINK_H

#include "cJSON.h"
#include "plat.h"
#include "util.h"

#include <stdbool.h>

typedef struct {
    na_sock fd;        /* NA_SOCK_NONE 表示未连接 */
    buf_t   in, out;
    int64_t last_rx;   /* 最近一次收到数据的时刻 */
} link_t;

/* addr：unix:/path、tcp:host:port 或 host:port。成功后为非阻塞。 */
int    link_connect(link_t *l, const char *addr, int timeout_ms, char *err, size_t errlen);
void   link_close(link_t *l);

void   link_send(link_t *l, cJSON *msg);    /* 序列化后放进发送缓冲，并释放 msg */
int    link_flush(link_t *l);               /* 尽量写出去；连接出错返回 -1 */
int    link_fill(link_t *l, int64_t now);   /* 读入可读的数据；对端关闭或出错返回 -1 */
cJSON *link_next(link_t *l, bool *bad);     /* 取出一条完整消息；没有返回 NULL，坏行时 *bad = true */

#endif
