/* update.c —— 内核自我更新，见 update.h */
#include "update.h"
#include "plat.h"
#include "util.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "version.h"

#ifdef _WIN32
#define EXE ".exe"
#else
#define EXE ""
#endif

/* 能被更新的文件：内核、执行器、rg */
static const char *const NAMES[] = { "nano-kernel" EXE, "nano-exec" EXE, "rg" EXE };
#define NNAMES (sizeof NAMES / sizeof NAMES[0])
#define PENDING "update-pending.json"
#define FAILED  "update-failed"

static int name_index(const char *name)
{
    for (size_t i = 0; i < NNAMES; i++)
        if (name && !strcmp(name, NAMES[i])) return (int)i;
    return -1;
}

static char *g_dir;   /* 内核所在的目录，启动时记下 */

void update_init(void)
{
    if (!g_dir) g_dir = plat_exe_dir();
}

/* 内核所在目录下的文件 */
static char *here(const char *name)
{
    update_init();
    return g_dir ? plat_join(g_dir, name) : NULL;
}

static bool exists(const char *p)
{
    plat_stat_t st;
    return p && plat_stat(p, &st) && st.exists;
}

static void remove_here(const char *name)
{
    char *p = here(name);
    if (p) plat_unlink(p);
    free(p);
}

static cJSON *read_pending(void)
{
    char *p = here(PENDING);
    buf_t b = {0};
    cJSON *m = NULL;
    if (p && plat_read_file(p, &b, 4096) == 0) m = cJSON_ParseWithLength(b.p ? b.p : "", b.len);
    buf_free(&b);
    free(p);
    return m;
}

static int write_here(const char *name, const char *text)
{
    char *p = here(name);
    int rc = p ? plat_write_atomic(p, text, strlen(text), 0600) : -1;
    free(p);
    return rc;
}

static int write_pending(const cJSON *m)
{
    char *s = cJSON_PrintUnformatted(m);
    int rc = write_here(PENDING, s);
    cJSON_free(s);
    return rc;
}

static const char *jstr(const cJSON *o, const char *key)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(o, key);
    return cJSON_IsString(v) ? v->valuestring : NULL;
}

bool update_on_start(bool *pending)
{
    *pending = false;
    cJSON *m = read_pending();
    if (!m) return false;
    const char *to = jstr(m, "to");
    if (!to || strcmp(to, NA_VERSION) != 0) {
        /* 标记不是给这个版本的（比如手动重装过），不管它 */
        cJSON_Delete(m);
        remove_here(PENDING);
        return false;
    }
    const cJSON *t = cJSON_GetObjectItemCaseSensitive(m, "tries");
    int tries = (cJSON_IsNumber(t) ? (int)t->valuedouble : 0) + 1;
    if (tries > UPDATE_MAX_TRIES) {
        cJSON_Delete(m);
        return update_rollback("启动了好几次都没能连上网关") == 0;
    }
    cJSON_DeleteItemFromObjectCaseSensitive(m, "tries");
    cJSON_AddNumberToObject(m, "tries", tries);
    write_pending(m);
    cJSON_Delete(m);
    *pending = true;
    return false;
}

void update_confirm(void)
{
    remove_here(PENDING);
    remove_here(FAILED);
    for (size_t i = 0; i < NNAMES; i++) {
        char *bad = xasprintf("%s.bad", NAMES[i]);
        remove_here(bad);
        free(bad);
    }
}

int update_rollback(const char *why)
{
    cJSON *m = read_pending();
    const char *from = m ? jstr(m, "from") : NULL;
    int rc = 0;
    for (size_t i = 0; i < NNAMES; i++) {
        char *cur = here(NAMES[i]);
        char *old = cur ? xasprintf("%s.old", cur) : NULL;
        if (old && exists(old)) {
            char *bad = xasprintf("%s.bad", cur);
            plat_unlink(bad);
            plat_rename(cur, bad);   /* 正在运行的程序在 Windows 上也能改名，只是不能删 */
            if (plat_rename(old, cur) < 0) {
                log_error("退回 %s 失败：%s", cur, strerror(errno));
                plat_rename(bad, cur);
                rc = -1;
            }
            free(bad);
        }
        free(old);
        free(cur);
    }
    if (rc == 0) {
        write_here(FAILED, NA_VERSION);
        remove_here(PENDING);
        log_warn("新版本 %s 没能正常运行（%s），已退回 %s", NA_VERSION, why, from ? from : "旧版本");
    }
    cJSON_Delete(m);
    return rc;
}

char *update_failed_version(void)
{
    char *p = here(FAILED);
    buf_t b = {0};
    char *v = NULL;
    if (p && plat_read_file(p, &b, 256) == 0 && b.len) {
        v = xstrdup(buf_cstr(&b));
        v[strcspn(v, "\r\n")] = 0;
    }
    buf_free(&b);
    free(p);
    return v;
}

/* 已经换好的文件改回去 */
static void undo_swaps(const bool *swapped)
{
    for (size_t i = 0; i < NNAMES; i++) {
        if (!swapped[i]) continue;
        char *cur = here(NAMES[i]);
        char *old = xasprintf("%s.old", cur);
        plat_unlink(cur);
        plat_rename(old, cur);
        free(old);
        free(cur);
    }
}

int update_apply(const cJSON *m, const char *exec, char *err, size_t errlen)
{
    const char *ver = jstr(m, "version");
    const cJSON *files = cJSON_GetObjectItemCaseSensitive(m, "files");
    if (!ver || !*ver || !cJSON_IsArray(files)) {
        snprintf(err, errlen, "更新消息不完整");
        return -1;
    }
    update_init();
    char *dir = g_dir ? xstrdup(g_dir) : NULL;
    char *exec_real = exec ? plat_canonical(exec) : NULL;
    char *exec_dir = exec_real ? plat_dirname(exec_real) : NULL;
    char *dir_real = dir ? plat_canonical(dir) : NULL;
    bool same = exec_dir && dir_real && plat_path_eq(exec_dir, dir_real);
    free(exec_real);
    free(exec_dir);
    free(dir_real);
    if (!same) {
        free(dir);
        snprintf(err, errlen, "内核和执行器不在同一个目录（不是安装脚本装的），不能自动更新；在这台电脑上重新运行安装脚本");
        return -1;
    }

    /* 1. 全部先写成 .new；有一个写不了就都不换 */
    bool written[NNAMES] = { false };
    int rc = 0;
    const cJSON *f;
    cJSON_ArrayForEach(f, files) {
        const char *name = jstr(f, "path"), *data = jstr(f, "data");
        int i = name_index(name);
        if (i < 0 || !data) {
            snprintf(err, errlen, "更新里有不认识的文件：%s", name ? name : "?");
            rc = -1;
            break;
        }
        size_t n = 0;
        unsigned char *bytes = b64_decode(data, &n);
        if (!bytes || n == 0) {
            free(bytes);
            snprintf(err, errlen, "%s 的内容坏了", name);
            rc = -1;
            break;
        }
        char *path = plat_join(dir, name);
        char *tmp = xasprintf("%s.new", path);
        if (plat_write_atomic(tmp, (const char *)bytes, n, 0755) < 0) {
            snprintf(err, errlen, "写不了 %s：%s", tmp, strerror(errno));
            rc = -1;
        } else {
            written[i] = true;
        }
        free(tmp);
        free(path);
        free(bytes);
        if (rc < 0) break;
    }
    if (rc == 0 && (!written[0] || !written[1])) {
        snprintf(err, errlen, "更新里缺内核或执行器");
        rc = -1;
    }

    /* 2. 旧的改名成 .old，新的改名过来 */
    bool swapped[NNAMES] = { false };
    for (size_t i = 0; rc == 0 && i < NNAMES; i++) {
        if (!written[i]) continue;
        char *cur = plat_join(dir, NAMES[i]);
        char *old = xasprintf("%s.old", cur);
        char *nw = xasprintf("%s.new", cur);
        plat_unlink(old);
        bool had = exists(cur);
        if (had && plat_rename(cur, old) < 0) {
            snprintf(err, errlen, "改不了 %s 的名字：%s", cur, strerror(errno));
            rc = -1;
        } else if (plat_rename(nw, cur) < 0) {
            snprintf(err, errlen, "换不上新的 %s：%s", cur, strerror(errno));
            if (had) plat_rename(old, cur);
            rc = -1;
        } else {
            swapped[i] = had;
        }
        free(nw);
        free(old);
        free(cur);
    }
    if (rc < 0) {
        undo_swaps(swapped);
        for (size_t i = 0; i < NNAMES; i++) {
            char *p = plat_join(dir, NAMES[i]);
            char *nw = xasprintf("%s.new", p);
            plat_unlink(nw);
            free(nw);
            free(p);
        }
        free(dir);
        return -1;
    }
    free(dir);

    /* 3. 记下来：新版本启动后要在限定时间内被网关接受，否则退回 */
    cJSON *mk = cJSON_CreateObject();
    cJSON_AddStringToObject(mk, "from", NA_VERSION);
    cJSON_AddStringToObject(mk, "to", ver);
    cJSON_AddNumberToObject(mk, "tries", 0);
    write_pending(mk);
    cJSON_Delete(mk);
    remove_here(FAILED);
    log_info("已换上新版本 %s（旧的留成 .old），重启", ver);
    return 0;
}

void update_reexec(char **argv)
{
    char *self = here(NAMES[0]);
    if (self && plat_exec(self, argv) < 0 && errno != ENOSYS) log_error("重启自己失败：%s", strerror(errno));
    free(self);
}
