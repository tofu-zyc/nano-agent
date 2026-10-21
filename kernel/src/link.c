#include "link.h"

#include <errno.h>
#include <netdb.h>
#include <netinet/in.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

#define MAX_LINE (64u << 20)   /* 单条消息上限，防止对端发疯把内存吃光 */

/* 非阻塞 connect + poll 等待，避免远端不可达时卡很久 */
static int connect_wait(int fd, const struct sockaddr *sa, socklen_t len, int timeout_ms)
{
    if (set_nonblock(fd) < 0) return -1;
    if (connect(fd, sa, len) == 0) return 0;
    if (errno != EINPROGRESS) return -1;
    struct pollfd p = { .fd = fd, .events = POLLOUT };
    int n = poll(&p, 1, timeout_ms);
    if (n == 0) { errno = ETIMEDOUT; return -1; }
    if (n < 0) return -1;
    int soerr = 0;
    socklen_t sl = sizeof soerr;
    if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &soerr, &sl) < 0) return -1;
    if (soerr) { errno = soerr; return -1; }
    return 0;
}

static int connect_unix(const char *path, int timeout_ms, char *err, size_t errlen)
{
    struct sockaddr_un sa = { .sun_family = AF_UNIX };
    if (strlen(path) >= sizeof sa.sun_path) {
        snprintf(err, errlen, "socket 路径太长：%s", path);
        return -1;
    }
    strcpy(sa.sun_path, path);
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0 || connect_wait(fd, (struct sockaddr *)&sa, sizeof sa, timeout_ms) < 0) {
        snprintf(err, errlen, "连接 %s 失败：%s", path, strerror(errno));
        if (fd >= 0) close(fd);
        return -1;
    }
    return fd;
}

static int connect_tcp(const char *hostport, int timeout_ms, char *err, size_t errlen)
{
    char host[256];
    const char *port;
    if (hostport[0] == '[') {   /* [::1]:7700 */
        const char *rb = strchr(hostport, ']');
        if (!rb || rb[1] != ':' || (size_t)(rb - hostport - 1) >= sizeof host) goto bad;
        memcpy(host, hostport + 1, (size_t)(rb - hostport - 1));
        host[rb - hostport - 1] = 0;
        port = rb + 2;
    } else {
        const char *colon = strrchr(hostport, ':');
        if (!colon || (size_t)(colon - hostport) >= sizeof host) goto bad;
        memcpy(host, hostport, (size_t)(colon - hostport));
        host[colon - hostport] = 0;
        port = colon + 1;
    }

    struct addrinfo hints = { .ai_family = AF_UNSPEC, .ai_socktype = SOCK_STREAM }, *res;
    int gai = getaddrinfo(host, port, &hints, &res);
    if (gai) {
        snprintf(err, errlen, "解析 %s 失败：%s", hostport, gai_strerror(gai));
        return -1;
    }
    int fd = -1, last = 0;
    for (struct addrinfo *ai = res; ai; ai = ai->ai_next) {
        fd = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
        if (fd < 0) { last = errno; continue; }
        if (connect_wait(fd, ai->ai_addr, ai->ai_addrlen, timeout_ms) == 0) break;
        last = errno;
        close(fd);
        fd = -1;
    }
    freeaddrinfo(res);
    if (fd < 0) {
        snprintf(err, errlen, "连接 %s 失败：%s", hostport, strerror(last));
        return -1;
    }
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &one, sizeof one);
    return fd;

bad:
    snprintf(err, errlen, "地址格式不对：%s（应为 host:port）", hostport);
    return -1;
}

int link_connect(link_t *l, const char *addr, int timeout_ms, char *err, size_t errlen)
{
    int fd;
    if (!strcmp(addr, "stdio")) {
        fd = dup(STDIN_FILENO);
        l->write_fd = dup(STDOUT_FILENO);
        if (fd < 0 || l->write_fd < 0) {
            snprintf(err, errlen, "打开标准输入输出失败：%s", strerror(errno));
            if (fd >= 0) close(fd);
            if (l->write_fd >= 0) close(l->write_fd);
            return -1;
        }
        set_nonblock(l->write_fd);
        set_cloexec(l->write_fd);
    } else if (!strncmp(addr, "unix:", 5)) fd = connect_unix(addr + 5, timeout_ms, err, errlen);
    else if (!strncmp(addr, "tcp:", 4)) fd = connect_tcp(addr + 4, timeout_ms, err, errlen);
    else fd = connect_tcp(addr, timeout_ms, err, errlen);
    if (fd < 0) return -1;
    if (strcmp(addr, "stdio")) l->write_fd = fd;
    set_nonblock(fd);
    set_cloexec(fd);
    l->fd = fd;
    l->in.len = l->out.len = 0;
    l->last_rx = now_ms();
    return 0;
}

void link_close(link_t *l)
{
    if (l->fd >= 0) {
        close(l->fd);
        if (l->write_fd != l->fd) close(l->write_fd);
    }
    l->fd = l->write_fd = -1;
    l->in.len = l->out.len = 0;
}

void link_send(link_t *l, cJSON *msg)
{
    char *s = cJSON_PrintUnformatted(msg);
    cJSON_Delete(msg);
    if (!s) return;
    if (l->fd >= 0) {
        buf_puts(&l->out, s);
        buf_append(&l->out, "\n", 1);
    }
    cJSON_free(s);
}

int link_flush(link_t *l)
{
    while (l->fd >= 0 && l->out.len) {
        ssize_t n = write(l->write_fd, l->out.p, l->out.len);
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
        ssize_t n = read(l->fd, tmp, sizeof tmp);
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
