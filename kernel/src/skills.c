/* skills.c —— 技能，见 skills.h */
#include "skills.h"
#include "util.h"

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define MAX_SKILLS   200
#define MAX_SKILL_MD (256 * 1024)
#define MAX_DESC     1024
#define MAX_FILES    60

static char *read_small(const char *path, size_t max)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return NULL;
    buf_t b = {0};
    char tmp[16384];
    ssize_t n;
    while ((n = read(fd, tmp, sizeof tmp)) > 0 && b.len < max) buf_append(&b, tmp, (size_t)n);
    close(fd);
    if (b.len > max) b.len = utf8_cut(b.p, b.len, max);
    return buf_cstr(&b);
}

static char *trim(char *s)
{
    while (*s == ' ' || *s == '\t') s++;
    size_t n = strlen(s);
    while (n && (s[n - 1] == ' ' || s[n - 1] == '\t' || s[n - 1] == '\r' || s[n - 1] == '\n')) s[--n] = 0;
    return s;
}

static char *unquote(char *s)
{
    size_t n = strlen(s);
    if (n >= 2 && ((s[0] == '"' && s[n - 1] == '"') || (s[0] == '\'' && s[n - 1] == '\''))) {
        s[n - 1] = 0;
        return s + 1;
    }
    return s;
}

/* 解析 SKILL.md：frontmatter 里的 name、description（支持 > 和 | 的多行写法），以及正文开始的位置 */
static void parse_md(char *md, char **name, char **desc, const char **body)
{
    *name = *desc = NULL;
    *body = md;
    if (strncmp(md, "---", 3) || (md[3] != '\n' && md[3] != '\r')) return;
    char *p = strchr(md, '\n');
    if (!p) return;
    p++;
    char *end = NULL;
    for (char *q = p; q && *q;) {
        if (!strncmp(q, "---", 3) && (q[3] == '\n' || q[3] == '\r' || !q[3])) { end = q; break; }
        q = strchr(q, '\n');
        if (q) q++;
    }
    if (!end) return;
    char *after = strchr(end, '\n');
    *body = after ? after + 1 : end + strlen(end);
    *end = 0;

    char *save = NULL;
    char *block_key = NULL;   /* 正在读多行值的键 */
    bool folded = false;
    buf_t block = {0};
    for (char *line = strtok_r(p, "\n", &save); line; line = strtok_r(NULL, "\n", &save)) {
        if (block_key && (line[0] == ' ' || line[0] == '\t')) {
            char *t = trim(line);
            if (block.len) buf_puts(&block, folded ? " " : "\n");
            buf_puts(&block, t);
            continue;
        }
        if (block_key) {
            char *v = buf_cstr(&block);
            if (!strcmp(block_key, "name") && !*name) *name = v;
            else if (!strcmp(block_key, "description") && !*desc) *desc = v;
            else free(v);
            block = (buf_t){0};
            block_key = NULL;
        }
        char *colon = strchr(line, ':');
        if (!colon || line[0] == ' ' || line[0] == '#') continue;
        *colon = 0;
        char *key = trim(line), *val = trim(colon + 1);
        if (strcmp(key, "name") && strcmp(key, "description")) continue;
        if (!*val || !strcmp(val, ">") || !strcmp(val, ">-") || !strcmp(val, "|") || !strcmp(val, "|-")) {
            block_key = key;
            folded = val[0] != '|';
            continue;
        }
        char **dst = !strcmp(key, "name") ? name : desc;
        if (!*dst) *dst = xstrdup(unquote(val));
    }
    if (block_key) {
        char *v = buf_cstr(&block);
        if (!strcmp(block_key, "name") && !*name) *name = v;
        else if (!strcmp(block_key, "description") && !*desc) *desc = v;
        else free(v);
    } else {
        buf_free(&block);
    }
}

static bool valid_name(const char *s)
{
    size_t n = strlen(s);
    if (!n || n > 64) return false;
    for (const char *p = s; *p; p++)
        if (!((*p >= 'a' && *p <= 'z') || (*p >= 'A' && *p <= 'Z') || (*p >= '0' && *p <= '9') ||
              *p == '-' || *p == '_' || *p == '.'))
            return false;
    return true;
}

static int cmp_skill(const void *a, const void *b)
{
    return strcmp(((const skill_t *)a)->name, ((const skill_t *)b)->name);
}

static int cmp_name(const void *a, const void *b)
{
    return strcmp(*(char *const *)a, *(char *const *)b);
}

static void free_list(skill_t *list, size_t n)
{
    for (size_t i = 0; i < n; i++) {
        free(list[i].name);
        free(list[i].description);
        free(list[i].dir);
    }
    free(list);
}

static uint64_t fnv(uint64_t h, const char *s)
{
    for (; *s; s++) {
        h ^= (unsigned char)*s;
        h *= 1099511628211ULL;
    }
    return h * 1099511628211ULL;
}

bool skills_scan(skills_t *s, const char *root)
{
    skill_t *list = NULL;
    size_t n = 0;
    char *real = root ? realpath(root, NULL) : NULL;
    DIR *d = real ? opendir(real) : NULL;
    if (d) {
        struct dirent *e;
        while ((e = readdir(d)) && n < MAX_SKILLS) {
            if (e->d_name[0] == '.') continue;
            char *dir = xasprintf("%s/%s", real, e->d_name);
            char *mdp = xasprintf("%s/SKILL.md", dir);
            char *md = read_small(mdp, MAX_SKILL_MD);
            free(mdp);
            if (!md) {
                free(dir);
                continue;
            }
            char *name, *desc;
            const char *body;
            parse_md(md, &name, &desc, &body);
            if (!name || !valid_name(name)) {
                free(name);
                name = valid_name(e->d_name) ? xstrdup(e->d_name) : NULL;
            }
            bool dup = false;
            for (size_t i = 0; name && i < n; i++) dup |= !strcmp(list[i].name, name);
            if (!name || dup) {
                if (name) log_warn("技能 %s 重名了，跳过 %s", name, dir);
                free(name);
                free(desc);
                free(md);
                free(dir);
                continue;
            }
            if (!desc || !*desc) {
                free(desc);
                desc = xstrdup("（没有简介）");
            }
            size_t dl = strlen(desc);
            if (dl > MAX_DESC) desc[utf8_cut(desc, dl, MAX_DESC)] = 0;
            list = xrealloc(list, (n + 1) * sizeof *list);
            list[n++] = (skill_t){ name, desc, dir };
            free(md);
        }
        closedir(d);
    }
    if (n > 1) qsort(list, n, sizeof *list, cmp_skill);
    uint64_t sig = 1469598103934665603ULL;
    sig = fnv(sig, real ? real : "");
    for (size_t i = 0; i < n; i++) {
        sig = fnv(sig, list[i].name);
        sig = fnv(sig, list[i].description);
    }
    bool changed = sig != s->sig;
    free_list(s->list, s->n);
    free(s->root);
    s->list = list;
    s->n = n;
    s->root = real;
    s->sig = sig;
    return changed;
}

void skills_free(skills_t *s)
{
    free_list(s->list, s->n);
    free(s->root);
    memset(s, 0, sizeof *s);
}

cJSON *skills_tool_spec(const skills_t *s)
{
    if (!s->n) return NULL;
    buf_t b = {0};
    buf_puts(&b, "使用一个技能（skill）。技能是事先写好的、做某一类事情的方法和配套脚本：调用后返回它的说明，照着做。"
                 "说明里提到的附带文件在技能目录里（只读），用 Read 读、用 Shell 运行，路径用返回结果里给的技能目录。"
                 "遇到下面这些事情时，先调用对应的技能再动手。可用的技能：");
    cJSON *names = cJSON_CreateArray();
    for (size_t i = 0; i < s->n; i++) {
        buf_puts(&b, "\n- ");
        buf_puts(&b, s->list[i].name);
        buf_puts(&b, "：");
        buf_puts(&b, s->list[i].description);
        cJSON_AddItemToArray(names, cJSON_CreateString(s->list[i].name));
    }
    cJSON *t = cJSON_CreateObject();
    cJSON_AddStringToObject(t, "name", "Skill");
    char *desc = buf_cstr(&b);
    cJSON_AddStringToObject(t, "description", desc);
    free(desc);
    cJSON *schema = cJSON_AddObjectToObject(t, "input_schema");
    cJSON_AddStringToObject(schema, "type", "object");
    cJSON *props = cJSON_AddObjectToObject(schema, "properties");
    cJSON *name = cJSON_AddObjectToObject(props, "name");
    cJSON_AddStringToObject(name, "type", "string");
    cJSON_AddStringToObject(name, "description", "技能的名字");
    cJSON_AddItemToObject(name, "enum", names);
    cJSON *req = cJSON_AddArrayToObject(schema, "required");
    cJSON_AddItemToArray(req, cJSON_CreateString("name"));
    return t;
}

cJSON *skills_json(const skills_t *s)
{
    cJSON *arr = cJSON_CreateArray();
    for (size_t i = 0; i < s->n; i++) {
        cJSON *o = cJSON_CreateObject();
        cJSON_AddStringToObject(o, "name", s->list[i].name);
        cJSON_AddStringToObject(o, "description", s->list[i].description);
        cJSON_AddItemToArray(arr, o);
    }
    return arr;
}

/* 列出技能目录里的附带文件（相对路径，最多两层、MAX_FILES 个） */
static void list_files(buf_t *b, const char *dir, const char *rel, int depth, size_t *count)
{
    char *path = rel[0] ? xasprintf("%s/%s", dir, rel) : xstrdup(dir);
    DIR *d = opendir(path);
    free(path);
    if (!d) return;
    char **names = NULL;
    size_t n = 0;
    struct dirent *e;
    while ((e = readdir(d))) {
        if (e->d_name[0] == '.') continue;
        names = xrealloc(names, (n + 1) * sizeof *names);
        names[n++] = xstrdup(e->d_name);
    }
    closedir(d);
    if (n > 1) qsort(names, n, sizeof *names, cmp_name);
    for (size_t i = 0; i < n; i++) {
        char *sub = rel[0] ? xasprintf("%s/%s", rel, names[i]) : xstrdup(names[i]);
        char *full = xasprintf("%s/%s", dir, sub);
        struct stat st;
        if (stat(full, &st) == 0) {
            if (S_ISDIR(st.st_mode) && depth < 2) {
                list_files(b, dir, sub, depth + 1, count);
            } else if (S_ISREG(st.st_mode) && strcmp(sub, "SKILL.md")) {
                if (*count < MAX_FILES) {
                    buf_puts(b, *count ? "、" : "");
                    buf_puts(b, sub);
                } else if (*count == MAX_FILES) {
                    buf_puts(b, "……");
                }
                (*count)++;
            }
        }
        free(full);
        free(sub);
        free(names[i]);
    }
    free(names);
}

const skill_t *skills_find(const skills_t *s, const char *name)
{
    for (size_t i = 0; name && i < s->n; i++)
        if (!strcmp(s->list[i].name, name)) return &s->list[i];
    return NULL;
}

/* ---- 技能文件交给网关 ---- */

#define MAX_SHIP_BYTES (4u << 20)
#define MAX_SHIP_FILES 500

static const char B64[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

static char *base64(const unsigned char *p, size_t n)
{
    char *out = xmalloc((n + 2) / 3 * 4 + 1), *o = out;
    for (size_t i = 0; i < n; i += 3) {
        unsigned v = p[i] << 16 | (i + 1 < n ? p[i + 1] << 8 : 0) | (i + 2 < n ? p[i + 2] : 0);
        *o++ = B64[v >> 18 & 63];
        *o++ = B64[v >> 12 & 63];
        *o++ = i + 1 < n ? B64[v >> 6 & 63] : '=';
        *o++ = i + 2 < n ? B64[v & 63] : '=';
    }
    *o = 0;
    return out;
}

/* 读整个文件（可以是二进制），最多 max 字节 */
static char *read_all(const char *path, size_t max, size_t *len)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return NULL;
    buf_t b = {0};
    char tmp[16384];
    ssize_t n;
    while ((n = read(fd, tmp, sizeof tmp)) > 0 && b.len <= max) buf_append(&b, tmp, (size_t)n);
    close(fd);
    if (n < 0 || b.len > max) {
        buf_free(&b);
        errno = n < 0 ? errno : EFBIG;
        return NULL;
    }
    *len = b.len;
    return buf_cstr(&b);
}

/* 把技能目录里的文件（最多两层子目录，不跟符号链接）加进 arr；超出上限返回 -1 */
static int ship_dir(cJSON *arr, const char *dir, const char *rel, int depth, size_t *bytes, char *err, size_t errlen)
{
    char *path = rel[0] ? xasprintf("%s/%s", dir, rel) : xstrdup(dir);
    DIR *d = opendir(path);
    free(path);
    if (!d) return 0;
    int rc = 0;
    struct dirent *e;
    while (rc == 0 && (e = readdir(d))) {
        if (e->d_name[0] == '.') continue;
        char *sub = rel[0] ? xasprintf("%s/%s", rel, e->d_name) : xstrdup(e->d_name);
        char *full = xasprintf("%s/%s", dir, sub);
        struct stat st;
        if (lstat(full, &st) == 0 && S_ISDIR(st.st_mode) && depth < 2) {
            rc = ship_dir(arr, dir, sub, depth + 1, bytes, err, errlen);
        } else if (lstat(full, &st) == 0 && S_ISREG(st.st_mode)) {
            *bytes += (size_t)st.st_size;
            if (*bytes > MAX_SHIP_BYTES || cJSON_GetArraySize(arr) >= MAX_SHIP_FILES) {
                snprintf(err, errlen, "技能目录太大（最多 %u 个文件、共 %u MB），没法传到执行的机器上",
                         MAX_SHIP_FILES, MAX_SHIP_BYTES >> 20);
                rc = -1;
            } else {
                size_t len = 0;
                char *data = read_all(full, MAX_SHIP_BYTES, &len);
                if (!data) {
                    snprintf(err, errlen, "读不了技能文件 %s：%s", sub, strerror(errno));
                    rc = -1;
                } else {
                    cJSON *f = cJSON_CreateObject();
                    cJSON_AddStringToObject(f, "path", sub);
                    char *b64 = base64((const unsigned char *)data, len);
                    cJSON_AddStringToObject(f, "data", b64);
                    free(b64);
                    if (st.st_mode & 0100) cJSON_AddBoolToObject(f, "exec", true);
                    cJSON_AddItemToArray(arr, f);
                    free(data);
                }
            }
        }
        free(full);
        free(sub);
    }
    closedir(d);
    return rc;
}

cJSON *skill_files_json(const skill_t *sk, char *err, size_t errlen)
{
    cJSON *arr = cJSON_CreateArray();
    size_t bytes = 0;
    if (ship_dir(arr, sk->dir, "", 0, &bytes, err, errlen) < 0) {
        cJSON_Delete(arr);
        return NULL;
    }
    return arr;
}

char *skill_load(const skills_t *s, const char *name, const char *dir, bool *ok)
{
    *ok = false;
    const skill_t *sk = skills_find(s, name);
    if (!sk) {
        buf_t b = {0};
        buf_puts(&b, "没有这个技能：");
        buf_puts(&b, name ? name : "（空）");
        buf_puts(&b, s->n ? "。可用的技能见 Skill 工具的说明" : "。这台机器上没有技能");
        return buf_cstr(&b);
    }
    char *mdp = xasprintf("%s/SKILL.md", sk->dir);
    char *md = read_small(mdp, MAX_SKILL_MD);
    free(mdp);
    if (!md) return xasprintf("读不了技能 %s 的说明：%s", sk->name, strerror(errno));
    char *n2, *d2;
    const char *body = md;
    parse_md(md, &n2, &d2, &body);
    free(n2);
    free(d2);

    buf_t b = {0};
    buf_puts(&b, "技能 ");
    buf_puts(&b, sk->name);
    buf_puts(&b, "\n技能目录：");
    buf_puts(&b, dir ? dir : sk->dir);
    buf_puts(&b, "（只读；说明里提到的文件都在这个目录下，路径里的 ~ 表示执行机器上的 HOME）");
    buf_t files = {0};
    size_t count = 0;
    list_files(&files, sk->dir, "", 0, &count);
    if (count) {
        buf_puts(&b, "\n附带文件：");
        buf_append(&b, files.p, files.len);
    }
    buf_free(&files);
    buf_puts(&b, "\n\n");
    while (*body == '\n' || *body == '\r') body++;
    buf_puts(&b, body);
    free(md);
    *ok = true;
    return buf_cstr(&b);
}
