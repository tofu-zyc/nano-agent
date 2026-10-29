/* test_watch.c —— 文件观察表：去抖、集合替换、重连缺口和队列溢出后的 resync */
#include "../src/watch.h"

#include "cJSON.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(__linux__)
#include <dirent.h>
#include <fcntl.h>
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

static void touch(const char *dir, const char *name)
{
    char path[1300];
    snprintf(path, sizeof path, "%s/%s", dir, name);
    FILE *f = fopen(path, "w");
    if (f) { fputs("x", f); fclose(f); }
}

int main(void)
{
    checks = failures = 0;
#if !defined(__linux__)
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

    /* 去抖：同一个窗口里的多次写入只出一条，计数合并 */
    touch(dir, "a");
    touch(dir, "a");
    na_watch_ready(w, 1000);
    na_watch_tick(w, 1000, on_event, NULL);
    CHECK(ngot == 0);                     /* 还没到点 */
    na_watch_tick(w, 1600, on_event, NULL);
    CHECK(ngot == 1);
    CHECK(got[0].count >= 2 && strcmp(got[0].key, "w1") == 0 && !got[0].resync);

    /* 目录登记会带上里面变化的文件名 */
    ngot = 0;
    touch(dir, "b");
    na_watch_ready(w, 2000);
    na_watch_tick(w, 2600, on_event, NULL);
    CHECK(ngot == 1 && strstr(got[0].path, "/b") != NULL);

    /* 重连后重新下发：现有登记各报一次缺口，不让处理器以为一直没变 */
    ngot = 0;
    list = watch_list(dir);
    na_watch_sync(w, list, true, 3000);
    cJSON_Delete(list);
    na_watch_tick(w, 3000, on_event, NULL);
    CHECK(ngot == 1 && got[0].resync);

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
        na_watch_ready(w, at);
        ngot = 0;
        na_watch_tick(w, at + 600, on_event, NULL);
        for (int i = 0; i < ngot; i++) if (got[i].resync) overflow = true;
        at += 700;
    }
    CHECK(overflow);
    CHECK(na_watch_next(w) == -1);        /* 报过之后不再重复 */

    /* 集合里拿掉的登记不再观察 */
    ngot = 0;
    list = cJSON_CreateArray();
    na_watch_sync(w, list, false, at);
    cJSON_Delete(list);
    touch(dir, "c");
    na_watch_ready(w, at + 1000);
    na_watch_tick(w, at + 1600, on_event, NULL);
    CHECK(ngot == 0);

    na_watch_free(w);
    DIR *d = opendir(dir);
    if (d) {
        struct dirent *e;
        while ((e = readdir(d))) {
            if (e->d_name[0] == '.') continue;
            char path[1300];
            snprintf(path, sizeof path, "%s/%s", dir, e->d_name);
            unlink(path);
        }
        closedir(d);
    }
    rmdir(dir);
#endif
    printf("watch: %d checks, %d failures\n", checks, failures);
    return failures != 0;
}
