/* test_watch.c —— 文件观察表：去抖、集合替换、重连缺口和队列溢出后的 resync */
#include "../src/watch.h"

#include "cJSON.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(__linux__) || defined(__APPLE__)
#include <dirent.h>
#include <fcntl.h>
#include <poll.h>
#include <stdlib.h>
#include <unistd.h>
#endif

static int failures, checks;

#define CHECK(cond) do { \
    checks++; \
    if (!(cond)) { failures++; fprintf(stderr, "%s:%d: CHECK failed: %s\n", __FILE__, __LINE__, #cond); } \
} while (0)

typedef struct {
    char key[96];
    char path[1024];
    int  count;
    bool resync;
} seen;

static seen got[64];
static int  ngot;

static void on_event(void *ud, const char *key, const char *path, int count, bool resync)
{
    (void)ud;
    if (ngot >= (int)(sizeof got / sizeof got[0])) return;
    snprintf(got[ngot].key, sizeof got[ngot].key, "%s", key);
    snprintf(got[ngot].path, sizeof got[ngot].path, "%s", path);
    got[ngot].count = count;
    got[ngot].resync = resync;
    ngot++;
}

static cJSON *watch_list(const char *path)
{
    cJSON *list = cJSON_CreateArray();
    cJSON *item = cJSON_CreateObject();
    cJSON_AddStringToObject(item, "key", "w1");
    cJSON_AddStringToObject(item, "path", path);
    cJSON_AddItemToArray(list, item);
    return list;
}

/* 等主循环该醒的那一下：Linux 的 inotify fd 和 macOS 的管道读端都能 poll。
 * 不先等就会在 macOS 上抢在后台线程前面读，读到空。 */
static void wait_ready(na_watch *w, int64_t now, int timeout_ms)
{
    intptr_t fd = na_watch_fd(w);
#if defined(__linux__) || defined(__APPLE__)
    if (fd >= 0) {
        struct pollfd p;
        p.fd = (int)fd;
        p.events = POLLIN;
        p.revents = 0;
        poll(&p, 1, timeout_ms);
    }
#endif
    (void)fd;
    (void)timeout_ms;
    na_watch_ready(w, now);
}

static void touch(const char *dir, const char *name)
{
    char path[1300];
    snprintf(path, sizeof path, "%s/%s", dir, name);
    FILE *f = fopen(path, "w");
    if (f) { fputs("x", f); fclose(f); }
}

/* 等到至少一条事件出现（或者超时）：macOS 的事件由工作线程异步送进来，
 * 不能像 inotify 那样"写完立刻读就一定有"。去抖窗口 500ms，按 200ms 步进推进。 */
static int collect(na_watch *w, int64_t now, int timeout_ms)
{
    ngot = 0;
    int64_t deadline = now + timeout_ms;
    for (; now <= deadline; now += 200) {
        wait_ready(w, now, 50);
        na_watch_tick(w, now, on_event, NULL);
        if (ngot > 0) return ngot;
    }
    return ngot;
}

int main(void)
{
    checks = failures = 0;
#if !defined(__linux__) && !defined(__APPLE__)
    printf("watch: 这个平台没有实现，跳过\n");
    return 0;
#else
    char dir[] = "/tmp/na-watch-XXXXXX";
    if (!mkdtemp(dir)) { perror("mkdtemp"); return 1; }
    na_watch *w = na_watch_new();
    CHECK(na_watch_supported(w));
    cJSON *list = watch_list(dir);
    na_watch_sync(w, list, false, 0);
    cJSON_Delete(list);
    CHECK(ngot == 0);                     /* 第一次登记不代表有变化 */

    /* 去抖：同一个窗口里的多次写入只出一条，计数合并（inotify 带逐条计数，kqueue 只报目录变了） */
    touch(dir, "a");
    touch(dir, "a");
    int n = collect(w, 1000, 2000);
    CHECK(n == 1);
#if defined(__linux__)
    CHECK(got[0].count >= 2 && strcmp(got[0].key, "w1") == 0 && !got[0].resync);
#else
    CHECK(got[0].count >= 1 && strcmp(got[0].key, "w1") == 0 && !got[0].resync);
#endif

    /* 目录登记会带上里面变化的文件名（kqueue 只报目录本身，不报子项名字） */
    ngot = 0;
    touch(dir, "b");
    n = collect(w, 3000, 2000);
#if defined(__linux__)
    CHECK(n == 1 && strstr(got[0].path, "/b") != NULL);
#else
    CHECK(n >= 1 && strcmp(got[0].key, "w1") == 0);
#endif

    /* 重连后重新下发：现有登记各报一次缺口，不让处理器以为一直没变 */
    ngot = 0;
    list = watch_list(dir);
    na_watch_sync(w, list, true, 3200);
    cJSON_Delete(list);
    na_watch_tick(w, 3200, on_event, NULL);
    CHECK(ngot == 1 && got[0].resync);

#if defined(__linux__)
    /* 队列溢出：没人读的时候堆满，之后从队列里读到 IN_Q_OVERFLOW 也要报缺口 */
    long limit = 16384;
    FILE *lf = fopen("/proc/sys/fs/inotify/max_queued_events", "r");
    if (lf) { if (fscanf(lf, "%ld", &limit) != 1) limit = 16384; fclose(lf); }
    char name[40];
    for (long i = 0; i < limit + 1024; i++) {
        snprintf(name, sizeof name, "f%ld", i);
        char path[1300];
        snprintf(path, sizeof path, "%s/%s", dir, name);
        int fd = creat(path, 0600);
        if (fd >= 0) close(fd);
    }
    bool overflow = false;
    int64_t at = 4000;
    for (int round = 0; round < 400 && !overflow; round++) {
        wait_ready(w, at, 1000);
        ngot = 0;
        na_watch_tick(w, at + 600, on_event, NULL);
        for (int i = 0; i < ngot; i++) if (got[i].resync) overflow = true;
        at += 700;
    }
    CHECK(overflow);
    CHECK(na_watch_next(w) == -1);        /* 报过之后不再重复 */
#else
    /* macOS：kqueue 没有等价的内核队列溢出；验证"改文件名"（重命名）也能报出来 */
    touch(dir, "rename-me");
    collect(w, 4000, 2000);
    ngot = 0;
    char from[1300], to[1300];
    snprintf(from, sizeof from, "%s/rename-me", dir);
    snprintf(to, sizeof to, "%s/renamed", dir);
    rename(from, to);
    n = collect(w, 6000, 2000);
    CHECK(n >= 1 && strcmp(got[0].key, "w1") == 0);
#endif

    /* 删掉前面的登记之后，保留下来的那条必须继续收到事件（槽位身份稳定） */
    {
        char dir2[] = "/tmp/na-watch-XXXXXX";
        if (mkdtemp(dir2)) {
            int64_t t0 = 8000;
            cJSON *two = cJSON_CreateArray();
            cJSON *a = cJSON_CreateObject();
            cJSON_AddStringToObject(a, "key", "keep-a");
            cJSON_AddStringToObject(a, "path", dir);
            cJSON_AddItemToArray(two, a);
            cJSON *b = cJSON_CreateObject();
            cJSON_AddStringToObject(b, "key", "keep-b");
            cJSON_AddStringToObject(b, "path", dir2);
            cJSON_AddItemToArray(two, b);
            na_watch_sync(w, two, false, t0);
            cJSON_Delete(two);
            /* 只留下第二条：第一条被摘掉，第二条的槽位不能跟着搬 */
            cJSON *one = cJSON_CreateArray();
            cJSON *b2 = cJSON_CreateObject();
            cJSON_AddStringToObject(b2, "key", "keep-b");
            cJSON_AddStringToObject(b2, "path", dir2);
            cJSON_AddItemToArray(one, b2);
            na_watch_sync(w, one, false, t0 + 100);
            cJSON_Delete(one);
            na_watch_tick(w, t0 + 200, on_event, NULL);
            ngot = 0;
            touch(dir2, "after-remove");
            n = collect(w, t0 + 400, 2000);
            CHECK(n >= 1 && strcmp(got[0].key, "keep-b") == 0);
            /* 同 key 换路径：新路径要报，旧路径不再报 */
            cJSON *moved = cJSON_CreateArray();
            cJSON *m = cJSON_CreateObject();
            cJSON_AddStringToObject(m, "key", "keep-b");
            cJSON_AddStringToObject(m, "path", dir);
            cJSON_AddItemToArray(moved, m);
            na_watch_sync(w, moved, false, t0 + 3000);
            cJSON_Delete(moved);
            na_watch_tick(w, t0 + 3200, on_event, NULL);
            ngot = 0;
            touch(dir2, "old-path");
            collect(w, t0 + 3400, 1500);   /* 旧路径：等一会儿，不该有事件 */
            CHECK(ngot == 0);
            ngot = 0;
            touch(dir, "new-path");
            n = collect(w, t0 + 5000, 2000);
            CHECK(n >= 1 && strcmp(got[0].key, "keep-b") == 0);
            rmdir(dir2);
        }
    }

    /* 集合里拿掉的登记不再观察 */
    ngot = 0;
    int64_t at2 = 12000;
    list = cJSON_CreateArray();
    na_watch_sync(w, list, false, at2);
    cJSON_Delete(list);
    touch(dir, "c");
    collect(w, at2 + 200, 1500);
    CHECK(ngot == 0);

    na_watch_free(w);
    printf("watch: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
#endif
}
