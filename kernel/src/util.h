/* util.h —— 日志、内存、缓冲区等小工具 */
#ifndef NA_UTIL_H
#define NA_UTIL_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#define PRINTF_LIKE(a, b) __attribute__((format(printf, a, b)))

enum { NA_LOG_DEBUG, NA_LOG_INFO, NA_LOG_WARN, NA_LOG_ERROR };
extern int na_log_level;

PRINTF_LIKE(2, 3) void na_log(int level, const char *fmt, ...);
#define log_debug(...) na_log(NA_LOG_DEBUG, __VA_ARGS__)
#define log_info(...)  na_log(NA_LOG_INFO, __VA_ARGS__)
#define log_warn(...)  na_log(NA_LOG_WARN, __VA_ARGS__)
#define log_error(...) na_log(NA_LOG_ERROR, __VA_ARGS__)

void *xmalloc(size_t n);
void *xcalloc(size_t n, size_t sz);
void *xrealloc(void *p, size_t n);
char *xstrdup(const char *s);
PRINTF_LIKE(1, 2) char *xasprintf(const char *fmt, ...);

/* 可增长的字节缓冲区 */
typedef struct {
    char  *p;
    size_t len, cap;
} buf_t;

void  buf_append(buf_t *b, const void *data, size_t n);
void  buf_puts(buf_t *b, const char *s);
void  buf_consume(buf_t *b, size_t n);   /* 丢掉开头 n 字节 */
char *buf_cstr(buf_t *b);                /* 保证以 \0 结尾，返回内部指针 */
void  buf_free(buf_t *b);

int64_t now_ms(void);                     /* 单调时钟，毫秒 */

/* 不超过 max 字节、且末尾不是半个 UTF-8 字符的前缀长度 */
size_t utf8_cut(const char *s, size_t len, size_t max);

/* 追加时把不合法的 UTF-8 字节（包括 \0）替换成 U+FFFD，保证发出去的 JSON 合法 */
void buf_put_utf8(buf_t *b, const char *s, size_t len);

/* base64 解码（忽略换行和 =）。出错返回 NULL；结果是 malloc 的，*n 是长度。 */
unsigned char *b64_decode(const char *s, size_t *n);
/* base64 编码（标准字母表，带 =），追加到 b */
void b64_encode(buf_t *b, const unsigned char *p, size_t n);

int set_nonblock(int fd);
int set_cloexec(int fd);

#endif
