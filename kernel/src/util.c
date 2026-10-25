#include "util.h"
#include "plat.h"

#include <fcntl.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

int na_log_level = NA_LOG_INFO;

void na_log(int level, const char *fmt, ...)
{
    static const char *names[] = { "DEBUG", "INFO", "WARN", "ERROR" };
    if (level < na_log_level) return;

    char ts[32];
    time_t t = time(NULL);
    struct tm tm;
#if NA_WINDOWS
    localtime_s(&tm, &t);
#else
    localtime_r(&t, &tm);
#endif
    strftime(ts, sizeof ts, "%Y-%m-%d %H:%M:%S", &tm);

    va_list ap;
    va_start(ap, fmt);
    fprintf(stderr, "%s %-5s ", ts, names[level]);
    vfprintf(stderr, fmt, ap);
    fputc('\n', stderr);
    va_end(ap);
}

static void oom(void)
{
    fputs("nano-kernel: out of memory\n", stderr);
    abort();
}

void *xmalloc(size_t n)
{
    void *p = malloc(n ? n : 1);
    if (!p) oom();
    return p;
}

void *xcalloc(size_t n, size_t sz)
{
    void *p = calloc(n ? n : 1, sz ? sz : 1);
    if (!p) oom();
    return p;
}

void *xrealloc(void *p, size_t n)
{
    p = realloc(p, n ? n : 1);
    if (!p) oom();
    return p;
}

char *xstrdup(const char *s)
{
    if (!s) return NULL;
    size_t n = strlen(s);
    char *p = xmalloc(n + 1);
    memcpy(p, s, n + 1);
    return p;
}

char *xasprintf(const char *fmt, ...)
{
    char small[256];
    va_list ap;
    va_start(ap, fmt);
    int n = vsnprintf(small, sizeof small, fmt, ap);
    va_end(ap);
    if (n < 0) return xstrdup("");
    if ((size_t)n < sizeof small) return xstrdup(small);
    char *p = xmalloc((size_t)n + 1);
    va_start(ap, fmt);
    vsnprintf(p, (size_t)n + 1, fmt, ap);
    va_end(ap);
    return p;
}

static void buf_reserve(buf_t *b, size_t extra)
{
    if (b->len + extra + 1 <= b->cap) return;
    size_t cap = b->cap ? b->cap : 256;
    while (cap < b->len + extra + 1) cap *= 2;
    b->p = xrealloc(b->p, cap);
    b->cap = cap;
}

void buf_append(buf_t *b, const void *data, size_t n)
{
    buf_reserve(b, n);
    memcpy(b->p + b->len, data, n);
    b->len += n;
    b->p[b->len] = 0;
}

void buf_puts(buf_t *b, const char *s)
{
    buf_append(b, s, strlen(s));
}

void buf_consume(buf_t *b, size_t n)
{
    if (n >= b->len) {
        b->len = 0;
    } else {
        memmove(b->p, b->p + n, b->len - n);
        b->len -= n;
    }
    if (b->p) b->p[b->len] = 0;
}

char *buf_cstr(buf_t *b)
{
    buf_reserve(b, 0);
    b->p[b->len] = 0;
    return b->p;
}

void buf_free(buf_t *b)
{
    free(b->p);
    b->p = NULL;
    b->len = b->cap = 0;
}

int64_t now_ms(void)
{
    return plat_now_ms();
}

size_t utf8_cut(const char *s, size_t len, size_t max)
{
    size_t n = len < max ? len : max;
    /* 往回找最后一个字符的首字节，看这个字符是否完整 */
    size_t i = n, cont = 0;
    while (i > 0 && cont < 3 && ((unsigned char)s[i - 1] & 0xC0) == 0x80) {
        i--;
        cont++;
    }
    if (i == 0) return n;
    unsigned char lead = (unsigned char)s[i - 1];
    size_t need = lead < 0x80 ? 1
                : (lead & 0xE0) == 0xC0 ? 2
                : (lead & 0xF0) == 0xE0 ? 3
                : (lead & 0xF8) == 0xF0 ? 4 : 1;
    return cont + 1 < need ? i - 1 : n;
}

/* 一个合法 UTF-8 字符的字节数；不合法返回 0 */
static size_t utf8_char_len(const unsigned char *p, size_t left)
{
    unsigned char c = p[0];
    if (c >= 0x01 && c < 0x80) return 1;
    size_t n;
    unsigned char lo = 0x80, hi = 0xBF;   /* 第二个字节的取值范围 */
    if (c >= 0xC2 && c <= 0xDF) n = 2;
    else if (c >= 0xE0 && c <= 0xEF) {
        n = 3;
        if (c == 0xE0) lo = 0xA0;         /* 过长编码 */
        if (c == 0xED) hi = 0x9F;         /* 代理区 */
    } else if (c >= 0xF0 && c <= 0xF4) {
        n = 4;
        if (c == 0xF0) lo = 0x90;
        if (c == 0xF4) hi = 0x8F;
    } else {
        return 0;
    }
    if (left < n || p[1] < lo || p[1] > hi) return 0;
    for (size_t i = 2; i < n; i++)
        if ((p[i] & 0xC0) != 0x80) return 0;
    return n;
}

void buf_put_utf8(buf_t *b, const char *s, size_t len)
{
    const unsigned char *p = (const unsigned char *)s;
    size_t i = 0;
    while (i < len) {
        size_t n = utf8_char_len(p + i, len - i);
        if (n) {
            buf_append(b, p + i, n);
            i += n;
        } else {
            buf_append(b, "\xEF\xBF\xBD", 3);   /* U+FFFD */
            i++;
        }
    }
}

int set_nonblock(int fd)
{
#if NA_WINDOWS
    (void)fd;
    return -1;
#else
    int fl = fcntl(fd, F_GETFL);
    return fl < 0 ? -1 : fcntl(fd, F_SETFL, fl | O_NONBLOCK);
#endif
}

int set_cloexec(int fd)
{
#if NA_WINDOWS
    (void)fd;
    return -1;
#else
    int fl = fcntl(fd, F_GETFD);
    return fl < 0 ? -1 : fcntl(fd, F_SETFD, fl | FD_CLOEXEC);
#endif
}

static int unb64(int ch)
{
    if (ch >= 'A' && ch <= 'Z') return ch - 'A';
    if (ch >= 'a' && ch <= 'z') return ch - 'a' + 26;
    if (ch >= '0' && ch <= '9') return ch - '0' + 52;
    if (ch == '+') return 62;
    if (ch == '/') return 63;
    return -1;
}

void b64_encode(buf_t *b, const unsigned char *p, size_t n)
{
    static const char tbl[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    char q[4];
    for (size_t i = 0; i < n; i += 3) {
        unsigned v = (unsigned)p[i] << 16;
        if (i + 1 < n) v |= (unsigned)p[i + 1] << 8;
        if (i + 2 < n) v |= p[i + 2];
        q[0] = tbl[v >> 18 & 63];
        q[1] = tbl[v >> 12 & 63];
        q[2] = i + 1 < n ? tbl[v >> 6 & 63] : '=';
        q[3] = i + 2 < n ? tbl[v & 63] : '=';
        buf_append(b, q, 4);
    }
}

unsigned char *b64_decode(const char *s, size_t *n)
{
    size_t len = strlen(s);
    unsigned char *out = xmalloc(len / 4 * 3 + 3);
    size_t o = 0;
    unsigned v = 0;
    int bits = 0;
    for (size_t i = 0; i < len; i++) {
        if (s[i] == '=' || s[i] == '\n' || s[i] == '\r') continue;
        int d = unb64((unsigned char)s[i]);
        if (d < 0) { free(out); return NULL; }
        v = v << 6 | (unsigned)d;
        bits += 6;
        if (bits >= 8) {
            bits -= 8;
            out[o++] = (unsigned char)(v >> bits);
        }
    }
    *n = o;
    return out;
}
