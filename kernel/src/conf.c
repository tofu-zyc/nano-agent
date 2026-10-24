#include "conf.h"
#include "plat.h"
#include "util.h"

#include <ctype.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

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

const char *const KCONF_KEYS[] = { "gateway", "token", "node", "exec", "skills", "max_steps",
                                    "run_timeout", "approval_timeout", "max_output", NULL };

/* 值里夹着另一个配置项（"65536gateway = …"）：两行粘在了一起，返回第二项开始的位置 */
const char *kconf_glued(const char *val)
{
    for (const char *const *k = KCONF_KEYS; *k; k++) {
        size_t n = strlen(*k);
        for (const char *q = strstr(val, *k); q; q = strstr(q + 1, *k)) {
            const char *e = q + n;
            while (*e == ' ' || *e == '\t') e++;
            if (*e == '=' && q > val) return q;
        }
    }
    return NULL;
}

/* 节点名会出现在网关和网页上：不能有空白、控制字符、冒号和斜杠，最长 64 字节 */
int kconf_load(kconf *k, const char *path, char *err, size_t errlen)
{
    memset(k, 0, sizeof *k);
    k->gateway = xstrdup(NA_WINDOWS ? "tcp:127.0.0.1:7700" : "unix:/tmp/nano-agent.sock");
    k->max_steps = 16;
    k->run_timeout = 600;
    k->approval_timeout = 300;
    k->max_output = 64 * 1024;

    buf_t text = {0};
    if (plat_read_file(path, &text, 1u << 20) < 0) {
        snprintf(err, errlen, "打开 %s 失败：%s", path, strerror(errno));
        buf_free(&text);
        return -1;
    }

    int lineno = 0, rc = 0;
    char *p = text.p ? text.p : "";
    if (!strncmp(p, "\xEF\xBB\xBF", 3)) p += 3;   /* 记事本存的 UTF-8 开头可能带 BOM */
    char *start = p;
    while (*p || p == start) {
        char *nl = strchr(p, '\n');
        if (nl) *nl = 0;
        lineno++;
        char *s = trim(p);
        if (!*s || *s == '#') {
            if (!nl) break;
            p = nl + 1;
            continue;
        }

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

        if (kconf_glued(val)) {
            snprintf(err, errlen, "%s 第 %d 行：%s = %s 好像是两行粘在了一起（少了换行），"
                     "把后面的配置项挪到下一行；或者运行 nano-kernel connect 网关地址，会顺手修好",
                     path, lineno, key, val);
            rc = -1;
            break;
        }
        long n = 0;
        if (!strcmp(key, "gateway")) set_str(&k->gateway, val);
        else if (!strcmp(key, "token")) set_str(&k->token, val);
        else if (!strcmp(key, "node")) set_str(&k->node, val);
        else if (!strcmp(key, "exec")) set_str(&k->exec, val);
        else if (!strcmp(key, "skills")) set_str(&k->skills, val);
        else if (!strcmp(key, "remote") || !strcmp(key, "ssh")) {
            snprintf(err, errlen, "%s 第 %d 行：%s 已经不用了（SSH 服务器改由网关连接，在网页「设备」里添加），"
                     "删掉这一行即可", path, lineno, key);
            rc = -1;
            break;
        }
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
        if (!nl) break;
        p = nl + 1;
    }
    buf_free(&text);
    if (rc) return rc;

    if (!k->node) k->node = plat_hostname();
    char *base = plat_dirname(path);
    char *real = plat_canonical(base);
    k->dir = real ? real : xstrdup(base);
    /* 没写 token：用配对得到的口令；还没配对过就是空的，连上网关后请求配对 */
    k->token_file = plat_join(base, "paired.token");
    if (!k->token || !*k->token) {
        buf_t tb = {0};
        if (plat_read_file(k->token_file, &tb, 4096) == 0) {
            char *t = trim(tb.p ? tb.p : "");
            if (*t) {
                set_str(&k->token, t);
                k->paired = true;
            }
        }
        buf_free(&tb);
    }
    if (!k->token) k->token = xstrdup("");
    if (k->exec && !plat_is_abs(k->exec)) {
        char *full = plat_join(base, k->exec);
        free(k->exec);
        k->exec = full;
    }
    free(base);
    if (!k->exec) {
        /* 默认：和内核程序放在同一个目录 */
        char *d = plat_exe_dir();
        k->exec = d ? plat_join(d, NA_WINDOWS ? "nano-exec.exe" : "nano-exec")
                    : xstrdup(NA_WINDOWS ? "nano-exec.exe" : "nano-exec");
        free(d);
    }
    return 0;
}

void kconf_free(kconf *k)
{
    free(k->skills);
    free(k->gateway);
    free(k->token);
    free(k->token_file);
    free(k->node);
    free(k->exec);
    free(k->dir);
    memset(k, 0, sizeof *k);
}
