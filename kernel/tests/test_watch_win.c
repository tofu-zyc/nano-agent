/* test_watch_win.c —— Windows 文件观察：目录/文件登记、过滤、摘除、缺口和关闭回收 */
#include "../src/watch.h"

#include "cJSON.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <winsock2.h>
#include <ws2tcpip.h>
#include <windows.h>
#include <direct.h>

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

static char dir[MAX_PATH];

static int64_t ms_now(void) { return (int64_t)GetTickCount64(); }

static cJSON *watch_list_key(const char *key, const char *path)
{
    cJSON *list = cJSON_CreateArray();
    cJSON *item = cJSON_CreateObject();
    cJSON_AddStringToObject(item, "key", key);
    cJSON_AddStringToObject(item, "path", path);
    cJSON_AddItemToArray(list, item);
    return list;
}

static void touch(const char *name)
{
    char path[MAX_PATH];
    snprintf(path, sizeof path, "%s\\%s", dir, name);
    FILE *f = fopen(path, "w");
    if (f) {
        fputs("x", f);
        fclose(f);
    }
}

/* 等主循环该醒的那一下：WSAPoll 等观察 fd，收到就 na_watch_ready。 */
static bool wait_ready(na_watch *w, int timeout_ms)
{
    intptr_t fd = na_watch_fd(w);
    if (fd < 0) return false;
    WSAPOLLFD p;
    memset(&p, 0, sizeof p);
    p.fd = (SOCKET)(uintptr_t)fd;
    p.events = POLLRDNORM;
    int n = WSAPoll(&p, 1, timeout_ms);
    if (n <= 0) return false;
    na_watch_ready(w, ms_now());
    return true;
}

/* 收集事件：等唤醒最多 2 秒，再多给一点把抖动窗口跨过去。 */
static int collect(na_watch *w)
{
    int64_t start = ms_now();
    while (ms_now() - start < 3000) {
        ngot = 0;
        (void)wait_ready(w, 0); /* 顺手把唤醒路径也走一遍（结果不影响这里，读队列才是断言依据） */
        na_watch_ready(w, ms_now());
        na_watch_tick(w, ms_now(), on_event, NULL);
        if (ngot > 0) return ngot;
        Sleep(20);
    }
    na_watch_tick(w, ms_now(), on_event, NULL);
    return ngot;
}

/* 看门狗：环境里观察不到通知时也要按时退出，不能把 CI 挂住。 */
static DWORD WINAPI watchdog(LPVOID ud)
{
    (void)ud;
    Sleep(20000);
    fprintf(stderr, "watch-win: 20 秒没有结束（环境可能没有送达通知），强制退出\n");
    ExitProcess(3);
    return 0;
}

int main(void)
{
    checks = failures = 0;
    CreateThread(NULL, 0, watchdog, NULL, 0, NULL);
    char tmp[MAX_PATH];
    DWORD n = GetTempPathA(MAX_PATH, tmp);
    if (!n || n > MAX_PATH - 32) { fprintf(stderr, "no temp path\n"); return 1; }
    snprintf(dir, sizeof dir, "%sna-watch-%lu", tmp, (unsigned long)GetTickCount());
    _mkdir(dir);

    na_watch *w = na_watch_new();
    CHECK(na_watch_supported(w));
    CHECK(na_watch_fd(w) >= 0);

    /* 目录登记：写入后能看到变化，路径带文件名 */
    cJSON *list = watch_list_key("w1", dir);
    na_watch_sync(w, list, false, ms_now());
    cJSON_Delete(list);
    CHECK(ngot == 0);
    touch("a.txt");
    CHECK(collect(w) == 1);
    CHECK(got[0].count >= 1 && strcmp(got[0].key, "w1") == 0 && !got[0].resync);
    CHECK(strstr(got[0].path, "a.txt") != NULL);

    /* 明确文件：只认这个名字，同目录别的文件不报 */
    touch("one.txt");
    char one[MAX_PATH];
    snprintf(one, sizeof one, "%s\\one.txt", dir);
    list = watch_list_key("file1", one);
    na_watch_sync(w, list, false, ms_now());
    cJSON_Delete(list);
    na_watch_tick(w, ms_now(), on_event, NULL);
    ngot = 0;
    touch("one.txt");
    collect(w);
    bool saw_one = false;
    for (int i = 0; i < ngot; i++) if (strcmp(got[i].key, "file1") == 0) saw_one = true;
    CHECK(saw_one);
    ngot = 0;
    touch("other.txt");
    collect(w);
    for (int i = 0; i < ngot; i++) CHECK(strcmp(got[i].key, "file1") != 0);

    /* 删掉前面的登记之后，保留下来的那条必须继续收到事件（槽位身份稳定） */
    {
        char dir2[MAX_PATH];
        snprintf(dir2, sizeof dir2, "%sna-watch-two-%lu", tmp, (unsigned long)GetTickCount());
        _mkdir(dir2);
        cJSON *two = cJSON_CreateArray();
        cJSON *a = cJSON_CreateObject();
        cJSON_AddStringToObject(a, "key", "keep-a");
        cJSON_AddStringToObject(a, "path", dir);
        cJSON_AddItemToArray(two, a);
        cJSON *b = cJSON_CreateObject();
        cJSON_AddStringToObject(b, "key", "keep-b");
        cJSON_AddStringToObject(b, "path", dir2);
        cJSON_AddItemToArray(two, b);
        na_watch_sync(w, two, false, ms_now());
        cJSON_Delete(two);
        cJSON *only = cJSON_CreateArray();
        cJSON *b2 = cJSON_CreateObject();
        cJSON_AddStringToObject(b2, "key", "keep-b");
        cJSON_AddStringToObject(b2, "path", dir2);
        cJSON_AddItemToArray(only, b2);
        na_watch_sync(w, only, false, ms_now());
        cJSON_Delete(only);
        char f2[MAX_PATH];
        snprintf(f2, sizeof f2, "%s\\after-remove.txt", dir2);
        FILE *fh = fopen(f2, "w");
        if (fh) { fputs("x", fh); fclose(fh); }
        ngot = 0;
        collect(w);
        bool saw_keep = false;
        for (int k = 0; k < ngot; k++) if (strcmp(got[k].key, "keep-b") == 0) saw_keep = true;
        CHECK(saw_keep);
        /* 同 key 换路径：旧路径不再报 */
        cJSON *moved = cJSON_CreateArray();
        cJSON *m = cJSON_CreateObject();
        cJSON_AddStringToObject(m, "key", "keep-b");
        cJSON_AddStringToObject(m, "path", dir);
        cJSON_AddItemToArray(moved, m);
        na_watch_sync(w, moved, false, ms_now());
        cJSON_Delete(moved);
        na_watch_tick(w, ms_now(), on_event, NULL);
        char f3[MAX_PATH];
        snprintf(f3, sizeof f3, "%s\\old-path.txt", dir2);
        FILE *oh = fopen(f3, "w");
        if (oh) { fputs("x", oh); fclose(oh); }
        ngot = 0;
        collect(w);
        CHECK(ngot == 0);
        _unlink(f2);
        _unlink(f3);
        _rmdir(dir2);
    }

    /* 重新下发带 resync：每一条各报一次缺口 */
    list = watch_list_key("w1", dir);
    na_watch_sync(w, list, true, ms_now());
    cJSON_Delete(list);
    na_watch_tick(w, ms_now() + 1000, on_event, NULL);
    bool resync = false;
    for (int i = 0; i < ngot; i++) if (got[i].resync) resync = true;
    CHECK(resync);

    /* 摘掉登记之后不再观察 */
    list = cJSON_CreateArray();
    na_watch_sync(w, list, false, ms_now());
    cJSON_Delete(list);
    ngot = 0;
    touch("after.txt");
    collect(w);
    CHECK(ngot == 0);

    /* 关闭要能回收工作线程和套接字（能返回就是回收了） */
    na_watch_free(w);

    /* 删除临时目录 */
    char path[MAX_PATH];
    snprintf(path, sizeof path, "%s\\a.txt", dir); _unlink(path);
    snprintf(path, sizeof path, "%s\\one.txt", dir); _unlink(path);
    snprintf(path, sizeof path, "%s\\other.txt", dir); _unlink(path);
    snprintf(path, sizeof path, "%s\\after.txt", dir); _unlink(path);
    _rmdir(dir);
    printf("watch-win: %d checks, %d failures\n", checks, failures);
    return failures != 0;
}
