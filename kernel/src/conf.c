#include "conf.h"
#include "util.h"

#include <ctype.h>
#include <errno.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <unistd.h>

static char *trim(char *s)
{
    while (isspace((unsigned char)*s)) s++;
    char *e = s + strlen(s);
    while (e > s && isspace((unsigned char)e[-1])) *--e = 0;
    return s;
}

static void set_str(char **dst, const char *v)
{
    free(*dst);
    *dst = xstrdup(v);
}

static int parse_int(const char *v, long lo, long hi, long *out)
{
    char *end;
    errno = 0;
    long n = strtol(v, &end, 10);
    if (errno || end == v || *end || n < lo || n > hi) return -1;
    *out = n;
    return 0;
}

/* 配置文件所在目录，不带结尾的 / */
static char *dir_of(const char *path)
{
    const char *slash = strrchr(path, '/');
    if (!slash) return xstrdup(".");
    if (slash == path) return xstrdup("/");
    char *d = xstrdup(path);
    d[slash - path] = 0;
    return d;
}

int kconf_load(kconf *k, const char *path, char *err, size_t errlen)
{
    memset(k, 0, sizeof *k);
    k->gateway = xstrdup("unix:/tmp/nano-agent.sock");
    k->max_steps = 16;
    k->run_timeout = 600;
    k->approval_timeout = 300;
    k->max_output = 64 * 1024;

    FILE *f = fopen(path, "r");
    if (!f) {
        snprintf(err, errlen, "打开 %s 失败：%s", path, strerror(errno));
        return -1;
    }

    char *line = NULL;
    size_t cap = 0;
    int lineno = 0, rc = 0;
    while (getline(&line, &cap, f) >= 0) {
        lineno++;
        char *s = trim(line);
        if (!*s || *s == '#') continue;

        char *eq = strchr(s, '=');
        if (!eq) {
            snprintf(err, errlen, "%s 第 %d 行：缺少 '='", path, lineno);
            rc = -1;
            break;
        }
        *eq = 0;
        char *key = trim(s), *val = trim(eq + 1);
        size_t vl = strlen(val);
        if (vl >= 2 && val[0] == '"' && val[vl - 1] == '"') {
            val[vl - 1] = 0;
            val++;
        }

        long n = 0;
        if (!strcmp(key, "gateway")) set_str(&k->gateway, val);
        else if (!strcmp(key, "token")) set_str(&k->token, val);
        else if (!strcmp(key, "node")) set_str(&k->node, val);
        else if (!strcmp(key, "exec")) set_str(&k->exec, val);
        else if (!strcmp(key, "tools_dir") || !strcmp(key, "admin")) {
            snprintf(err, errlen, "%s 第 %d 行：%s 已经不用了（工具改成了内置的；管理员改在网关配置的 "
                     "access.admins 里设置），删掉这一行即可", path, lineno, key);
            rc = -1;
            break;
        }
        else if (!strcmp(key, "max_steps") && !parse_int(val, 1, 1000, &n)) k->max_steps = (int)n;
        else if (!strcmp(key, "run_timeout") && !parse_int(val, 1, 86400, &n)) k->run_timeout = (int)n;
        else if (!strcmp(key, "approval_timeout") && !parse_int(val, 1, 86400, &n)) k->approval_timeout = (int)n;
        else if (!strcmp(key, "max_output") && !parse_int(val, 1024, 64L << 20, &n)) k->max_output = (size_t)n;
        else {
            snprintf(err, errlen, "%s 第 %d 行：未知配置项或取值不合法：%s = %s", path, lineno, key, val);
            rc = -1;
            break;
        }
    }
    free(line);
    fclose(f);
    if (rc) return rc;

    if (!k->token || !*k->token) {
        snprintf(err, errlen, "%s：必须设置 token（与网关配置里的 kernel.token 一致）", path);
        return -1;
    }
    if (!k->node) {
        char host[256] = "kernel";
        gethostname(host, sizeof host - 1);
        k->node = xstrdup(host);
    }
    char *base = dir_of(path);
    k->dir = realpath(base, NULL);
    if (!k->dir) k->dir = xstrdup(base);
    if (k->exec && k->exec[0] != '/') {
        char *full = xasprintf("%s/%s", base, k->exec);
        free(k->exec);
        k->exec = full;
    }
    free(base);
    if (!k->exec) {
        /* 默认：和内核程序放在同一个目录 */
        char self[4096];
        ssize_t n = readlink("/proc/self/exe", self, sizeof self - 1);
        if (n > 0) {
            self[n] = 0;
            char *d = dir_of(self);
            k->exec = xasprintf("%s/nano-exec", d);
            free(d);
        } else {
            k->exec = xstrdup("nano-exec");
        }
    }
    return 0;
}

void kconf_free(kconf *k)
{
    free(k->gateway);
    free(k->token);
    free(k->node);
    free(k->exec);
    free(k->dir);
    memset(k, 0, sizeof *k);
}
