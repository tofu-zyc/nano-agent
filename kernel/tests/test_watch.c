/* test_watch.c —— 文件观察表：去抖、集合替换、重连缺口和队列溢出后的 resync */
#include "../src/watch.h"

#include "cJSON.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(__linux__) || defined(__APPLE__)
#include <dirent.h>
#include <fcntl.h>
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

    /* 去抖：同一个窗口里的多次写入只出一条，计数合并 */
    touch(dir, "a");
    touch(dir, "a");
    na_watch_ready(w, 1000);
    na_watch_tick(w, 1000, on_event, NULL);
    CHECK(ngot == 0);                     /* 还没到点 */
    na_watch_tick(w, 1600, on_event, NULL);
    CHECK(ngot == 1);
    CHECK(got[0].count >= 2 && strcmp(got[0].key, "w1") == 0 && !got[0].resync);

    /* 目录登记会带上里面变化的文件名（kqueue 只报目录本身，不报子项名字） */
    ngot = 0;
    touch(dir, "b");
    na_watch_ready(w, 2000);
    na_watch_tick(w, 2600, on_event, NULL);
#if defined(__linux__)
    CHECK(ngot == 1 && strstr(got[0].path, "/b") != NULL);
#else
    CHECK(ngot == 1 && strcmp(got[0].key, "w1") == 0);
#endif

    /* 重连后重新下发：现有登记各报一次缺口，不让处理器以为一直没变 */
    ngot = 0;
    list = watch_list(dir);
    na_watch_sync(w, list, true, 3000);
    cJSON_Delete(list);
    na_watch_tick(w, 3000, on_event, NULL);
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
        na_watch_ready(w, at);
        ngot = 0;
        na_watch_tick(w, at + 600, on_event, NULL);
        for (int i = 0; i < ngot; i++) if (got[i].resync) overflow = true;
        at += 700;
    }
    CHECK(overflow);
    CHECK(na_watch_next(w) == -1);        /* 报过之后不再重复 */
#else
    /* macOS：kqueue 没有等价的内核队列溢出；这里验证"改文件名"（重命名）也能报出来 */
    int64_t at = 4000;
    na_watch_tick(w, at, on_event, NULL);
    ngot = 0;
    char from[1300], to[1300];
    snprintf(from, sizeof from, "%s/rename-me", dir);
    snprintf(to, sizeof to, "%s/renamed", dir);
    touch(dir, "rename-me");
    na_watch_ready(w, at);
    na_watch_tick(w, at + 600, on_event, NULL);
    ngot = 0;
    rename(from, to);
    na_watch_ready(w, at + 800);
    na_watch_tick(w, at + 1400, on_event, NULL);
    CHECK(ngot >= 1 && strcmp(got[0].key, "w1") == 0);
#endif

    /* 删掉前面的登记之后，保留下来的那条必须继续收到事件（槽位身份稳定） */
    {
        char dir2[] = "/tmp/na-watch-XXXXXX";
        if (mkdtemp(dir2)) {
            cJSON *two = cJSON_CreateArray();
            cJSON *a = cJSON_CreateObject();
            cJSON_AddStringToObject(a, "key", "keep-a");
            cJSON_AddStringToObject(a, "path", dir);
            cJSON_AddItemToArray(two, a);
            cJSON *b = cJSON_CreateObject();
            cJSON_AddStringToObject(b, "key", "keep-b");
            cJSON_AddStringToObject(b, "path", dir2);
            cJSON_AddItemToArray(two, b);
            na_watch_sync(w, two, false, 5000);
            cJSON_Delete(two);
            /* 只留下第二条：第一条被摘掉，第二条的槽位不能跟着搬 */
            cJSON *one = cJSON_CreateArray();
            cJSON *b2 = cJSON_CreateObject();
            cJSON_AddStringToObject(b2, "key", "keep-b");
            cJSON_AddStringToObject(b2, "path", dir2);
            cJSON_AddItemToArray(one, b2);
            na_watch_sync(w, one, false, 5100);
            cJSON_Delete(one);
            ngot = 0;
            touch(dir2, "after-remove");
            na_watch_ready(w, 5200);
            na_watch_tick(w, 5900, on_event, NULL);
            CHECK(ngot == 1 && strcmp(got[0].key, "keep-b") == 0);
            /* 同 key 换路径：旧路径不再报，新路径要报 */
            cJSON *moved = cJSON_CreateArray();
            cJSON *m = cJSON_CreateObject();
            cJSON_AddStringToObject(m, "key", "keep-b");
            cJSON_AddStringToObject(m, "path", dir);
            cJSON_AddItemToArray(moved, m);
            na_watch_sync(w, moved, false, 6000);
            cJSON_Delete(moved);
            na_watch_tick(w, 6100, on_event, NULL);
            ngot = 0;
            touch(dir2, "old-path");
            na_watch_ready(w, 6200);
            na_watch_tick(w, 6900, on_event, NULL);
            CHECK(ngot == 0);
            ngot = 0;
            touch(dir, "new-path");
            na_watch_ready(w, 7000);
            na_watch_tick(w, 7700, on_event, NULL);
            CHECK(ngot == 1 && strcmp(got[0].key, "keep-b") == 0);
            rmdir(dir2);
        }
    }

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
