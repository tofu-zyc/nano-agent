#include "link.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define MAX_LINE (64u << 20)   /* 单条消息上限，防止对端发疯把内存吃光 */

int link_connect(link_t *l, const char *addr, int timeout_ms, char *err, size_t errlen)
{
    na_sock fd = NA_SOCK_NONE;
    if (plat_connect(addr, timeout_ms, &fd, err, errlen) < 0) return -1;
    l->fd = fd;
    l->in.len = l->out.len = 0;
    l->last_rx = now_ms();
    return 0;
}

void link_close(link_t *l)
{
    if (l->fd != NA_SOCK_NONE) plat_sock_close(l->fd);
    l->fd = NA_SOCK_NONE;
    l->in.len = l->out.len = 0;
}

void link_send(link_t *l, cJSON *msg)
{
    char *s = cJSON_PrintUnformatted(msg);
    cJSON_Delete(msg);
    if (!s) return;
    if (l->fd != NA_SOCK_NONE) {
        buf_puts(&l->out, s);
        buf_append(&l->out, "\n", 1);
    }
    cJSON_free(s);
}

int link_flush(link_t *l)
{
    while (l->fd != NA_SOCK_NONE && l->out.len) {
        long n = plat_sock_write(l->fd, l->out.p, l->out.len);
        if (n > 0) { buf_consume(&l->out, (size_t)n); continue; }
        if (n < 0 && errno == EINTR) continue;
        if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) return 0;
        return -1;
    }
    return 0;
}

int link_fill(link_t *l, int64_t now)
{
    char tmp[65536];
    for (;;) {
        long n = plat_sock_read(l->fd, tmp, sizeof tmp);
        if (n > 0) {
            buf_append(&l->in, tmp, (size_t)n);
            l->last_rx = now;
            if (l->in.len > MAX_LINE && !memchr(l->in.p, '\n', l->in.len)) return -1;
            continue;
        }
        if (n == 0) return -1;
        if (errno == EINTR) continue;
        if (errno == EAGAIN || errno == EWOULDBLOCK) return 0;
        return -1;
    }
}

cJSON *link_next(link_t *l, bool *bad)
{
    *bad = false;
    for (;;) {
        char *nl = l->in.len ? memchr(l->in.p, '\n', l->in.len) : NULL;
        if (!nl) return NULL;
        size_t n = (size_t)(nl - l->in.p);
        cJSON *msg = n ? cJSON_ParseWithLength(l->in.p, n) : NULL;
        buf_consume(&l->in, n + 1);
        if (n == 0) continue;   /* 空行 */
        if (!cJSON_IsObject(msg)) {
            cJSON_Delete(msg);
            *bad = true;
            return NULL;
        }
        return msg;
    }
}
