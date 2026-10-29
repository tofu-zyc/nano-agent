#include "watch.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(__linux__)
#include <sys/inotify.h>
#include <unistd.h>
#endif

#define WATCH_PATH_MAX 1024
#define WATCH_KEY_MAX  96

typedef struct {
    char    key[WATCH_KEY_MAX];
    char    path[WATCH_PATH_MAX];
    char    last[WATCH_PATH_MAX];   /* 最近一次变化的实际路径（目录登记时是里面的文件） */
    int     wd;
    int     count;
    int64_t due;                    /* 0 表示没有待发的事件 */
    int64_t next;                   /* 观察不到时下次再试的时刻 */
    bool    resync;
} watch_entry;

struct na_watch {
    int         fd;
    int         n;
    watch_entry items[NA_WATCH_MAX];
};

static const char *sget(const cJSON *o, const char *k)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(o, k);
    return cJSON_IsString(v) ? v->valuestring : NULL;
}

#if defined(__linux__)
#define WATCH_MASK (IN_MODIFY | IN_ATTRIB | IN_CLOSE_WRITE | IN_CREATE | IN_DELETE | \
                    IN_MOVED_FROM | IN_MOVED_TO | IN_DELETE_SELF | IN_MOVE_SELF)
#endif

na_watch *na_watch_new(void)
{
    na_watch *w = calloc(1, sizeof *w);
    if (!w) return NULL;
    w->fd = -1;
#if defined(__linux__)
    w->fd = inotify_init1(IN_NONBLOCK | IN_CLOEXEC);
#endif
    for (int i = 0; i < NA_WATCH_MAX; i++) w->items[i].wd = -1;
    return w;
}

void na_watch_free(na_watch *w)
{
    if (!w) return;
#if defined(__linux__)
    if (w->fd >= 0) close(w->fd);
#endif
    free(w);
}

bool na_watch_supported(const na_watch *w) { return w && w->fd >= 0; }
int  na_watch_fd(const na_watch *w) { return w ? w->fd : -1; }

/* 失败即返回 -1：路径不在、权限不够都算“现在观察不到”，由调用处按缺口报一次。 */
static int watch_attach(na_watch *w, watch_entry *e)
{
#if defined(__linux__)
    if (w->fd < 0) return -1;
    return inotify_add_watch(w->fd, e->path, WATCH_MASK);
#else
    (void)w; (void)e;
    return -1;
#endif
}

static void watch_detach(na_watch *w, watch_entry *e)
{
#if defined(__linux__)
    if (w->fd >= 0 && e->wd >= 0) inotify_rm_watch(w->fd, e->wd);
#else
    (void)w;
#endif
    e->wd = -1;
}

void na_watch_sync(na_watch *w, const cJSON *list, bool resync, int64_t now)
{
    if (!w) return;
    bool keep[NA_WATCH_MAX];
    memset(keep, 0, sizeof keep);
    const cJSON *it = NULL;
    cJSON_ArrayForEach(it, list) {
        const char *key = sget(it, "key"), *path = sget(it, "path");
        if (!key || !path || !*key || !*path) continue;
        if (strlen(key) >= WATCH_KEY_MAX || strlen(path) >= WATCH_PATH_MAX) continue;
        int slot = -1;
        for (int i = 0; i < w->n; i++) if (!strcmp(w->items[i].key, key)) { slot = i; break; }
        if (slot < 0) {
            if (w->n >= NA_WATCH_MAX) continue;   /* 超过上限的登记明确不接 */
            slot = w->n++;
            memset(&w->items[slot], 0, sizeof w->items[slot]);
            w->items[slot].wd = -1;
            snprintf(w->items[slot].key, sizeof w->items[slot].key, "%s", key);
        }
        watch_entry *e = &w->items[slot];
        keep[slot] = true;
        if (strcmp(e->path, path)) {
            watch_detach(w, e);
            snprintf(e->path, sizeof e->path, "%s", path);
            e->last[0] = 0;
        }
        if (e->wd < 0) {
            e->wd = watch_attach(w, e);
            if (e->wd < 0) e->next = now + NA_WATCH_RETRY;
        }
        if (resync) {
            e->resync = true;
            if (!e->due) e->due = now;
        }
    }
    int out = 0;
    for (int i = 0; i < w->n; i++) {
        if (!keep[i]) {
            watch_detach(w, &w->items[i]);
            continue;
        }
        if (out != i) w->items[out] = w->items[i];
        out++;
    }
    w->n = out;
}

/* 目录登记时把里面的文件名接到后面；超长就截断，报告用的路径不因此丢失。 */
static void join_path(char *out, size_t cap, const char *dir, const char *name)
{
    size_t d = strlen(dir);
    if (d > cap - 1) d = cap - 1;
    memcpy(out, dir, d);
    out[d] = 0;
    if (!name || !*name || d + 2 > cap) return;
    size_t room = cap - d - 2, n = strlen(name);
    if (n > room) n = room;
    out[d] = '/';
    memcpy(out + d + 1, name, n);
    out[d + 1 + n] = 0;
}

static watch_entry *watch_by_wd(na_watch *w, int wd)
{
    for (int i = 0; i < w->n; i++)
        if (w->items[i].wd >= 0 && w->items[i].wd == wd) return &w->items[i];
    return NULL;
}

void na_watch_ready(na_watch *w, int64_t now)
{
#if defined(__linux__)
    if (!w || w->fd < 0) return;
    int64_t due = now + NA_WATCH_DEBOUNCE;
    union { struct inotify_event ev; char buf[8192]; } u;
    for (int reads = 0; reads < 16; reads++) {
        ssize_t n = read(w->fd, u.buf, sizeof u.buf);
        if (n <= 0) return;
        for (char *p = u.buf; p < u.buf + n;) {
            const struct inotify_event *ev = (const struct inotify_event *)p;
            p += sizeof(struct inotify_event) + ev->len;
            if (ev->mask & IN_Q_OVERFLOW) {
                for (int i = 0; i < w->n; i++) {
                    w->items[i].resync = true;
                    w->items[i].due = due;
                }
                continue;
            }
            watch_entry *e = watch_by_wd(w, ev->wd);
            if (!e) continue;
            if (ev->mask & (IN_IGNORED | IN_DELETE_SELF | IN_MOVE_SELF | IN_UNMOUNT)) {
                e->wd = -1;                 /* 路径没了：按缺口报一次，之后定期重试 */
                e->next = now + NA_WATCH_RETRY;
                e->resync = true;
            }
            char full[WATCH_PATH_MAX];
            join_path(full, sizeof full, e->path, ev->len ? ev->name : NULL);
            memcpy(e->last, full, sizeof e->last);
            e->count++;
            e->due = due;
        }
    }
#else
    (void)w; (void)now;
#endif
}

void na_watch_tick(na_watch *w, int64_t now, na_watch_emit emit, void *ud)
{
    if (!w || !emit) return;
    for (int i = 0; i < w->n; i++) {
        watch_entry *e = &w->items[i];
        if (e->wd < 0 && e->next && now >= e->next) {
            e->wd = watch_attach(w, e);
            e->next = e->wd < 0 ? now + NA_WATCH_RETRY : 0;
        }
        if (!e->due || e->due > now) continue;
        emit(ud, e->key, e->last[0] ? e->last : e->path, e->count, e->resync);
        e->count = 0;
        e->resync = false;
        e->due = 0;
    }
}

int64_t na_watch_next(const na_watch *w)
{
    if (!w) return -1;
    int64_t next = -1;
    for (int i = 0; i < w->n; i++) {
        int64_t due = w->items[i].due;
        if (due && (next < 0 || due < next)) next = due;
    }
    return next;
}
