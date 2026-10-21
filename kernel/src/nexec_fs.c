/* nexec_fs.c —— 路径检查，以及 Read / Write / Edit */
#include "nexec.h"

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#define EDIT_MAX_FILE (32u << 20)   /* Edit 最多处理多大的文件 */
#define LIST_MAX      1000          /* 列目录最多列多少项 */

/* ---- 参数 ---- */

static const char *arg_str(const cJSON *a, const char *key)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(a, key);
    return cJSON_IsString(v) ? v->valuestring : NULL;
}

static long arg_int(const cJSON *a, const char *key, long def)
{
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(a, key);
    return cJSON_IsNumber(v) ? (long)v->valuedouble : def;
}

static void put_dirs(const xctx *x, buf_t *out)
{
    for (size_t i = 0; i < x->ndirs; i++) {
        if (i) buf_puts(out, "、");
        buf_puts(out, x->dirs[i]);
    }
    if (!x->ndirs) buf_puts(out, "（未设置）");
}

/* ---- 路径 ---- */

char *expand_home(const char *home, const char *p)
{
    if (p[0] == '~' && (p[1] == '/' || p[1] == 0)) return xasprintf("%s%s", home, p + 1);
    return xstrdup(p);
}

/* 按字面规范化绝对路径：合并多余的 /，去掉 . 和 .. */
static char *normalize(const char *abs)
{
    size_t n = strlen(abs);
    char *out = xmalloc(n + 2);
    size_t len = 0;
    const char *s = abs;
    while (*s) {
        while (*s == '/') s++;
        if (!*s) break;
        const char *e = strchr(s, '/');
        size_t cl = e ? (size_t)(e - s) : strlen(s);
        if (cl == 1 && s[0] == '.') {
            /* 跳过 */
        } else if (cl == 2 && s[0] == '.' && s[1] == '.') {
            while (len > 0 && out[len - 1] != '/') len--;
            if (len > 0) len--;   /* 去掉分隔的 / */
        } else {
            out[len++] = '/';
            memcpy(out + len, s, cl);
            len += cl;
        }
        s += cl;
    }
    if (len == 0) out[len++] = '/';
    out[len] = 0;
    return out;
}

char *path_resolve(const xctx *x, const char *p, buf_t *out)
{
    if (!p || !*p) {
        buf_puts(out, "缺少路径");
        return NULL;
    }
    if (p[0] != '/' && p[0] != '~' && !x->ndirs) {
        buf_puts(out, "还没有设置工作目录，没法解析相对路径。请让用户先用 /dir add <路径> 添加工作目录");
        return NULL;
    }
    char *full = p[0] == '/' ? xstrdup(p)
               : p[0] == '~' ? expand_home(x->home, p)
               : xasprintf("%s/%s", x->dirs[0], p);
    char *abs = normalize(full);
    free(full);

    /* 从完整路径开始往上找第一个存在的祖先，解析它的符号链接，再接上不存在的部分 */
    size_t cut = strlen(abs);
    for (;;) {
        char saved = abs[cut];
        abs[cut] = 0;
        char *real = realpath(cut ? abs : "/", NULL);
        abs[cut] = saved;
        if (real) {
            char *res = abs[cut] ? xasprintf("%s%s", strcmp(real, "/") ? real : "", abs + cut) : xstrdup(real);
            free(real);
            free(abs);
            return res;
        }
        if (errno != ENOENT) {
            buf_puts(out, "无法访问路径 ");
            buf_puts(out, abs);
            buf_puts(out, "：");
            buf_puts(out, errno == EACCES ? "没有权限（不在允许访问的范围内）" : strerror(errno));
            free(abs);
            return NULL;
        }
        while (cut > 0 && abs[cut - 1] != '/') cut--;
        if (cut > 0) cut--;
    }
}

bool path_inside(const char *path, const char *dir)
{
    size_t n = strlen(dir);
    if (n == 1 && dir[0] == '/') return true;
    return strncmp(path, dir, n) == 0 && (path[n] == 0 || path[n] == '/');
}

static bool inside_any(char *const *list, size_t n, const char *path)
{
    for (size_t i = 0; i < n; i++)
        if (path_inside(path, list[i])) return true;
    return false;
}

bool path_can_read(const xctx *x, const char *real, buf_t *out)
{
    if (inside_any(x->dirs, x->ndirs, real) || inside_any(x->ro, x->nro, real) || inside_any(x->rw, x->nrw, real))
        return true;
    buf_puts(out, "路径不在工作目录内：");
    buf_puts(out, real);
    buf_puts(out, "（工作目录：");
    put_dirs(x, out);
    buf_puts(out, "）。需要访问的话，请让用户用 /dir add 把它加为工作目录");
    return false;
}

bool path_can_write(const xctx *x, const char *real, buf_t *out)
{
    if ((x->write && inside_any(x->dirs, x->ndirs, real)) || inside_any(x->rw, x->nrw, real)) return true;
    if (!x->write && inside_any(x->dirs, x->ndirs, real)) {
        buf_puts(out, "当前是只读模式，不能修改文件");
        return false;
    }
    buf_puts(out, "路径不在工作目录内，不能写入：");
    buf_puts(out, real);
    buf_puts(out, "（工作目录：");
    put_dirs(x, out);
    buf_puts(out, "）。需要的话，请让用户用 /dir add 把它加为工作目录");
    return false;
}

/* ---- 文件读写 ---- */

static void put_errno(buf_t *out, const char *what, const char *path)
{
    buf_puts(out, what);
    buf_puts(out, " ");
    buf_puts(out, path);
    buf_puts(out, " 失败：");
    buf_puts(out, errno == EACCES ? "没有权限" : strerror(errno));
}

/* 读整个文件，超过 max 字节失败（errno = EFBIG） */
static int slurp(const char *path, size_t max, buf_t *b)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return -1;
    char tmp[65536];
    for (;;) {
        ssize_t n = read(fd, tmp, sizeof tmp);
        if (n < 0 && errno == EINTR) continue;
        if (n < 0) { int e = errno; close(fd); errno = e; return -1; }
        if (n == 0) break;
        if (b->len + (size_t)n > max) { close(fd); errno = EFBIG; return -1; }
        buf_append(b, tmp, (size_t)n);
    }
    close(fd);
    return 0;
}

static bool looks_binary(const char *p, size_t n)
{
    return memchr(p, 0, n < 8192 ? n : 8192) != NULL;
}

/* 原子地写文件：先写同目录下的临时文件再改名。已有文件保留原来的权限位。 */
static int write_atomic(const char *path, const char *data, size_t len)
{
    struct stat st;
    bool exists = stat(path, &st) == 0;
    const char *slash = strrchr(path, '/');
    char *tmp = xasprintf("%.*s/.%s.nano-XXXXXX", (int)(slash - path), path, slash + 1);
    int fd = mkstemp(tmp);
    if (fd < 0) { free(tmp); return -1; }
    mode_t mode;
    if (exists) {
        mode = st.st_mode & 07777;
    } else {
        mode_t um = umask(0);
        umask(um);
        mode = 0666 & ~um;
    }
    size_t off = 0;
    int rc = 0;
    while (off < len) {
        ssize_t n = write(fd, data + off, len - off);
        if (n < 0 && errno == EINTR) continue;
        if (n < 0) { rc = -1; break; }
        off += (size_t)n;
    }
    if (rc == 0 && fchmod(fd, mode) < 0) rc = -1;
    int saved = errno;
    if (close(fd) < 0 && rc == 0) { rc = -1; saved = errno; }
    if (rc == 0 && rename(tmp, path) < 0) { rc = -1; saved = errno; }
    if (rc) unlink(tmp);
    free(tmp);
    errno = saved;
    return rc;
}

/* mkdir -p path 的上级目录 */
static int make_parents(const char *path)
{
    char *p = xstrdup(path);
    for (char *s = p + 1; *s; s++) {
        if (*s != '/') continue;
        *s = 0;
        if (mkdir(p, 0777) < 0 && errno != EEXIST) { int e = errno; free(p); errno = e; return -1; }
        *s = '/';
    }
    free(p);
    return 0;
}

static size_t count_lines(const char *p, size_t n)
{
    size_t lines = 0;
    for (size_t i = 0; i < n; i++)
        if (p[i] == '\n') lines++;
    if (n && p[n - 1] != '\n') lines++;
    return lines;
}

/* ---- Read ---- */

static int cmp_str(const void *a, const void *b)
{
    return strcmp(*(char *const *)a, *(char *const *)b);
}

static bool list_dir(const char *path, buf_t *out)
{
    DIR *d = opendir(path);
    if (!d) { put_errno(out, "打开目录", path); return false; }
    char **names = NULL;
    size_t n = 0, total = 0;
    struct dirent *e;
    while ((e = readdir(d))) {
        if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
        total++;
        if (n >= LIST_MAX) continue;
        bool dir = e->d_type == DT_DIR;
        if (e->d_type == DT_UNKNOWN || e->d_type == DT_LNK) {
            struct stat st;
            dir = fstatat(dirfd(d), e->d_name, &st, 0) == 0 && S_ISDIR(st.st_mode);
        }
        names = xrealloc(names, (n + 1) * sizeof *names);
        names[n++] = xasprintf("%s%s", e->d_name, dir ? "/" : "");
    }
    closedir(d);
    qsort(names, n, sizeof *names, cmp_str);
    char head[PATH_MAX + 64];
    snprintf(head, sizeof head, "%s 是目录，共 %zu 项：\n", path, total);
    buf_puts(out, head);
    for (size_t i = 0; i < n; i++) {
        buf_put_utf8(out, names[i], strlen(names[i]));
        buf_puts(out, "\n");
        free(names[i]);
    }
    free(names);
    if (total > n) {
        char note[96];
        snprintf(note, sizeof note, "…（只列出前 %d 项）\n", LIST_MAX);
        buf_puts(out, note);
    }
    if (!total) buf_puts(out, "（空目录）\n");
    return true;
}

bool tool_read(const xctx *x, const cJSON *a, buf_t *out)
{
    char *real = path_resolve(x, arg_str(a, "path"), out);
    if (!real) return false;
    bool ok = false;
    struct stat st;
    if (!path_can_read(x, real, out)) goto done;
    if (stat(real, &st) < 0) { put_errno(out, "读取", real); goto done; }
    if (S_ISDIR(st.st_mode)) { ok = list_dir(real, out); goto done; }

    long offset = arg_int(a, "offset", 1), limit = arg_int(a, "limit", READ_DEFAULT_LIMIT);
    if (offset < 1) offset = 1;
    if (limit < 1) limit = READ_DEFAULT_LIMIT;

    FILE *f = fopen(real, "rb");
    if (!f) { put_errno(out, "读取", real); goto done; }
    char peek[8192];
    size_t np = fread(peek, 1, sizeof peek, f);
    if (looks_binary(peek, np)) {
        char note[PATH_MAX + 96];
        snprintf(note, sizeof note, "%s 是二进制文件（%lld 字节），不显示内容", real, (long long)st.st_size);
        buf_puts(out, note);
        fclose(f);
        ok = true;
        goto done;
    }
    rewind(f);

    char *line = NULL;
    size_t cap = 0;
    ssize_t len;
    long no = 0, shown_from = 0, shown_to = 0;
    bool full = false;   /* 输出到了上限 */
    while ((len = getline(&line, &cap, f)) >= 0) {
        no++;
        if (no < offset || no >= offset + limit || full) continue;
        if (len > 0 && line[len - 1] == '\n') len--;
        if (len > 0 && line[len - 1] == '\r') len--;
        char num[32];
        snprintf(num, sizeof num, "%6ld\t", no);
        size_t keep = utf8_cut(line, (size_t)len, READ_LINE_MAX);
        if (out->len + strlen(num) + keep + 64 > x->max_output) { full = true; continue; }
        buf_puts(out, num);
        buf_put_utf8(out, line, keep);
        if (keep < (size_t)len) {
            char more[64];
            snprintf(more, sizeof more, "…（本行共 %zd 字节）", len);
            buf_puts(out, more);
        }
        buf_puts(out, "\n");
        if (!shown_from) shown_from = no;
        shown_to = no;
    }
    free(line);
    fclose(f);

    char note[160];
    if (no == 0) {
        buf_puts(out, "（空文件）");
    } else if (!shown_from) {
        snprintf(note, sizeof note, "（文件只有 %ld 行，offset 超出了范围）", no);
        buf_puts(out, note);
    } else if (shown_from > 1 || shown_to < no) {
        snprintf(note, sizeof note, "（共 %ld 行，显示了第 %ld~%ld 行%s，其余部分用 offset/limit 继续读）",
                 no, shown_from, shown_to, full ? "，输出达到上限" : "");
        buf_puts(out, note);
    }
    ok = true;
done:
    free(real);
    return ok;
}

/* ---- Write ---- */

bool tool_write(const xctx *x, const cJSON *a, buf_t *out)
{
    const char *content = arg_str(a, "content");
    if (!content) {
        buf_puts(out, "缺少 content（字符串）");
        return false;
    }
    char *real = path_resolve(x, arg_str(a, "path"), out);
    if (!real) return false;
    bool ok = false;
    struct stat st;
    if (!path_can_write(x, real, out)) goto done;
    bool exists = stat(real, &st) == 0;
    if (exists && S_ISDIR(st.st_mode)) {
        buf_puts(out, real);
        buf_puts(out, " 是目录，不能当作文件写入");
        goto done;
    }
    size_t len = strlen(content);
    if (make_parents(real) < 0 || write_atomic(real, content, len) < 0) {
        put_errno(out, "写入", real);
        goto done;
    }
    char note[PATH_MAX + 96];
    snprintf(note, sizeof note, "已%s %s（%zu 行，%zu 字节）", exists ? "覆盖" : "创建", real,
             count_lines(content, len), len);
    buf_puts(out, note);
    ok = true;
done:
    free(real);
    return ok;
}

/* ---- Edit ---- */

/* 显示修改后第 line 行附近（含新内容）的几行，方便模型确认改对了 */
static void put_snippet(buf_t *out, const char *text, size_t len, size_t first, size_t nlines)
{
    size_t from = first > 3 ? first - 3 : 1, to = first + nlines + 2;
    if (to - from > 40) to = from + 40;
    size_t no = 1;
    const char *p = text, *end = text + len;
    while (p < end && no <= to) {
        const char *nl = memchr(p, '\n', (size_t)(end - p));
        size_t ll = nl ? (size_t)(nl - p) : (size_t)(end - p);
        if (no >= from) {
            char num[32];
            snprintf(num, sizeof num, "%6zu\t", no);
            buf_puts(out, num);
            buf_put_utf8(out, p, utf8_cut(p, ll, READ_LINE_MAX));
            buf_puts(out, "\n");
        }
        if (!nl) break;
        p = nl + 1;
        no++;
    }
}

bool tool_edit(const xctx *x, const cJSON *a, buf_t *out)
{
    const char *olds = arg_str(a, "old_string"), *news = arg_str(a, "new_string");
    const cJSON *all = cJSON_GetObjectItemCaseSensitive(a, "replace_all");
    if (!olds || !news) {
        buf_puts(out, "缺少 old_string 或 new_string（字符串）");
        return false;
    }
    if (!*olds) {
        buf_puts(out, "old_string 不能为空；要新建文件或整个重写请用 Write");
        return false;
    }
    if (!strcmp(olds, news)) {
        buf_puts(out, "old_string 和 new_string 一样，没有需要修改的");
        return false;
    }
    char *real = path_resolve(x, arg_str(a, "path"), out);
    if (!real) return false;
    bool ok = false;
    buf_t text = {0}, res = {0};
    if (!path_can_write(x, real, out)) goto done;
    if (slurp(real, EDIT_MAX_FILE, &text) < 0) {
        if (errno == EFBIG) buf_puts(out, "文件太大，Edit 不支持；可以用 Shell 里的 sed 之类处理");
        else put_errno(out, "读取", real);
        goto done;
    }
    if (looks_binary(text.p ? text.p : "", text.len)) {
        buf_puts(out, "这是二进制文件，Edit 只能改文本文件");
        goto done;
    }

    size_t ol = strlen(olds), nl = strlen(news), count = 0, first = 0;
    const char *base = buf_cstr(&text), *p = base, *hit;
    while ((hit = strstr(p, olds))) {
        if (!count) first = (size_t)(hit - base);
        count++;
        p = hit + ol;
    }
    if (count == 0) {
        buf_puts(out, "没有找到 old_string。缩进、空格、换行都要和文件里完全一致，可以先用 Read 看一下文件");
        goto done;
    }
    if (count > 1 && !cJSON_IsTrue(all)) {
        char note[200];
        snprintf(note, sizeof note, "old_string 在文件里出现了 %zu 次。请多带几行上下文让它只出现一次，"
                 "或者设 replace_all: true 全部替换", count);
        buf_puts(out, note);
        goto done;
    }

    p = base;
    size_t done_n = 0;
    while ((hit = strstr(p, olds)) && (done_n == 0 || cJSON_IsTrue(all))) {
        buf_append(&res, p, (size_t)(hit - p));
        buf_append(&res, news, nl);
        p = hit + ol;
        done_n++;
    }
    buf_puts(&res, p);
    if (write_atomic(real, res.p, res.len) < 0) {
        put_errno(out, "写入", real);
        goto done;
    }

    char note[PATH_MAX + 64];
    snprintf(note, sizeof note, "已修改 %s：替换了 %zu 处。修改后的片段：\n", real, done_n);
    buf_puts(out, note);
    size_t line = 1;
    for (size_t i = 0; i < first; i++)
        if (base[i] == '\n') line++;
    put_snippet(out, res.p, res.len, line, count_lines(news, nl));
    ok = true;
done:
    buf_free(&text);
    buf_free(&res);
    free(real);
    return ok;
}
