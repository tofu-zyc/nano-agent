/* nexec_fs.c —— 路径检查，以及 Read / Write / Edit */
#include "nexec.h"
#include "plat.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

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

char *path_resolve(const xctx *x, const char *p, buf_t *out)
{
    if (!p || !*p) {
        buf_puts(out, "缺少路径");
        return NULL;
    }
    if (!plat_is_abs(p) && p[0] != '~' && !x->ndirs && !x->home_only) {
        buf_puts(out, "还没有设置工作目录，没法解析相对路径。请让用户先用 /dir add <路径> 添加工作目录");
        return NULL;
    }
    char *full;
    if (plat_is_abs(p)) full = xstrdup(p);
    else if (p[0] == '~') full = plat_expand_home(p);
    else full = plat_join(x->ndirs ? x->dirs[0] : x->home, p);
    char *abs = plat_abspath(NULL, full);
    free(full);
    if (!abs) {
        buf_puts(out, "无法解析路径");
        return NULL;
    }
    char *real = plat_canonical(abs);
    if (!real) {
        buf_puts(out, "无法访问路径 ");
        buf_puts(out, abs);
        buf_puts(out, "：");
        buf_puts(out, errno == EACCES ? "没有权限（不在允许访问的范围内）" : strerror(errno));
        free(abs);
        return NULL;
    }
    free(abs);
    return real;
}

static bool inside_any(char *const *list, size_t n, const char *path)
{
    for (size_t i = 0; i < n; i++)
        if (plat_inside(path, list[i])) return true;
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
    buf_puts(out, "）。写工作目录外需要 elevated: true，会请用户批准");
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

static bool looks_binary(const char *p, size_t n)
{
    return memchr(p, 0, n < 8192 ? n : 8192) != NULL;
}

/* 按行读文件，跨平台的 getline 替代 */
typedef struct {
    plat_file *f;
    char       buf[65536];
    size_t     pos, len;
    bool       eof;
} line_iter;

static void li_open(line_iter *it, const char *path)
{
    memset(it, 0, sizeof *it);
    it->f = plat_open_read(path);
}

static bool li_next(line_iter *it, buf_t *line)
{
    bool got = false;
    line->len = 0;
    for (;;) {
        if (it->pos >= it->len) {
            if (it->eof) return got;
            long n = plat_file_read(it->f, it->buf, sizeof it->buf);
            if (n <= 0) { it->eof = true; return got; }
            it->len = (size_t)n;
            it->pos = 0;
        }
        char *nl = memchr(it->buf + it->pos, '\n', it->len - it->pos);
        if (nl) {
            size_t n = (size_t)(nl - (it->buf + it->pos));
            buf_append(line, it->buf + it->pos, n);
            it->pos += n + 1;
            return true;
        }
        buf_append(line, it->buf + it->pos, it->len - it->pos);
        it->pos = it->len;
        got = true;
    }
}

static void li_close(line_iter *it)
{
    plat_file_close(it->f);
    it->f = NULL;
}

static bool file_uses_crlf(const char *path)
{
    plat_file *f = plat_open_read(path);
    if (!f) return false;
    char buf[65536];
    long n = plat_file_read(f, buf, sizeof buf);
    plat_file_close(f);
    for (long i = 0; i + 1 < n; i++)
        if (buf[i] == '\r' && buf[i + 1] == '\n') return true;
    return false;
}

/* 先把 CRLF 统一成 LF，再按需要变成 CRLF */
static void convert_newlines(const char *s, size_t len, bool crlf, buf_t *out)
{
    for (size_t i = 0; i < len; i++) {
        if (s[i] == '\r' && i + 1 < len && s[i + 1] == '\n') i++;
        if (crlf && s[i] == '\n') buf_append(out, "\r\n", 2);
        else buf_append(out, s + i, 1);
    }
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
    plat_dirent *ents = NULL;
    size_t n = 0;
    bool more = false;
    char err[256] = "";
    if (!plat_read_dir(path, LIST_MAX, &ents, &n, &more, err, sizeof err)) {
        buf_puts(out, err);
        return false;
    }
    char **names = NULL;
    size_t nn = 0;
    for (size_t i = 0; i < n; i++) {
        names = xrealloc(names, (nn + 1) * sizeof *names);
        names[nn++] = xasprintf("%s%s", ents[i].name, ents[i].dir ? plat_sep() : "");
    }
    qsort(names, nn, sizeof *names, cmp_str);
    char head[160];
    snprintf(head, sizeof head, "%s 是目录，共 %zu 项：\n", path, nn);
    buf_puts(out, head);
    for (size_t i = 0; i < nn; i++) {
        buf_put_utf8(out, names[i], strlen(names[i]));
        buf_puts(out, "\n");
        free(names[i]);
    }
    free(names);
    plat_dirents_free(ents, n);
    if (more) {
        char note[96];
        snprintf(note, sizeof note, "…（只列出前 %d 项）\n", LIST_MAX);
        buf_puts(out, note);
    }
    if (!nn) buf_puts(out, "（空目录）\n");
    return true;
}

bool tool_read(const xctx *x, const cJSON *a, buf_t *out)
{
    char *real = path_resolve(x, arg_str(a, "path"), out);
    if (!real) return false;
    bool ok = false;
    plat_stat_t st;
    if (!path_can_read(x, real, out)) goto done;
    if (!plat_stat(real, &st)) { put_errno(out, "读取", real); goto done; }
    if (st.is_dir) { ok = list_dir(real, out); goto done; }

    long offset = arg_int(a, "offset", 1), limit = arg_int(a, "limit", READ_DEFAULT_LIMIT);
    if (offset < 1) offset = 1;
    if (limit < 1) limit = READ_DEFAULT_LIMIT;

    plat_file *peekf = plat_open_read(real);
    if (!peekf) { put_errno(out, "读取", real); goto done; }
    char peek[8192];
    long np = plat_file_read(peekf, peek, sizeof peek);
    plat_file_close(peekf);
    if (np > 0 && looks_binary(peek, (size_t)np)) {
        char note[160];
        snprintf(note, sizeof note, "%s 是二进制文件（%lld 字节），不显示内容", real, (long long)st.size);
        buf_puts(out, note);
        ok = true;
        goto done;
    }

    line_iter it;
    li_open(&it, real);
    if (!it.f) { put_errno(out, "读取", real); goto done; }
    buf_t line = {0};
    long no = 0, shown_from = 0, shown_to = 0;
    bool full = false;
    while (li_next(&it, &line)) {
        no++;
        if (no < offset || no >= offset + limit || full) continue;
        size_t len = line.len;
        if (len > 0 && line.p[len - 1] == '\r') len--;
        char num[32];
        snprintf(num, sizeof num, "%6ld\t", no);
        size_t keep = utf8_cut(line.p ? line.p : "", len, READ_LINE_MAX);
        if (out->len + strlen(num) + keep + 64 > x->max_output) { full = true; continue; }
        buf_puts(out, num);
        buf_put_utf8(out, line.p ? line.p : "", keep);
        if (keep < len) {
            char more[64];
            snprintf(more, sizeof more, "…（本行共 %zd 字节）", len);
            buf_puts(out, more);
        }
        buf_puts(out, "\n");
        if (!shown_from) shown_from = no;
        shown_to = no;
    }
    buf_free(&line);
    li_close(&it);

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
    plat_stat_t st;
    if (!path_can_write(x, real, out)) goto done;
    bool exists = plat_stat(real, &st);
    if (exists && st.is_dir) {
        buf_puts(out, real);
        buf_puts(out, " 是目录，不能当作文件写入");
        goto done;
    }
    size_t len = strlen(content);
    buf_t data = {0};
    if (exists && file_uses_crlf(real)) convert_newlines(content, len, true, &data);
    else buf_append(&data, content, len);
    char *dir = plat_dirname(real);
    if (plat_makedirs(dir) < 0 || plat_write_atomic(real, data.p ? data.p : "", data.len, 0) < 0) {
        put_errno(out, "写入", real);
        free(dir);
        buf_free(&data);
        goto done;
    }
    free(dir);
    char note[160];
    snprintf(note, sizeof note, "已%s %s（%zu 行，%zu 字节）", exists ? "覆盖" : "创建", real,
             count_lines(data.p ? data.p : "", data.len), data.len);
    buf_puts(out, note);
    buf_free(&data);
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
        if (ll && p[ll - 1] == '\r') ll--;
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
    buf_t text = {0}, res = {0}, needle = {0}, repl = {0};
    if (!path_can_write(x, real, out)) goto done;
    if (plat_read_file(real, &text, EDIT_MAX_FILE) < 0) {
        if (errno == EFBIG) buf_puts(out, "文件太大，Edit 不支持；可以用 Shell 里的 sed 之类处理");
        else put_errno(out, "读取", real);
        goto done;
    }
    if (looks_binary(text.p ? text.p : "", text.len)) {
        buf_puts(out, "这是二进制文件，Edit 只能改文本文件");
        goto done;
    }

    bool crlf = false;
    const char *base = text.p ? text.p : "";
    for (size_t i = 0; i + 1 < text.len; i++)
        if (base[i] == '\r' && base[i + 1] == '\n') { crlf = true; break; }
    if (crlf) {
        convert_newlines(olds, strlen(olds), true, &needle);
        convert_newlines(news, strlen(news), true, &repl);
    } else {
        buf_puts(&needle, olds);
        buf_puts(&repl, news);
    }
    const char *ol = needle.p ? needle.p : olds, *nl = repl.p ? repl.p : news;
    size_t olen = crlf ? needle.len : strlen(olds), nlen = crlf ? repl.len : strlen(news);

    size_t count = 0, first = 0;
    const char *p = base, *hit;
    while ((hit = strstr(p, ol))) {
        if (!count) first = (size_t)(hit - base);
        count++;
        p = hit + olen;
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
    while ((hit = strstr(p, ol)) && (done_n == 0 || cJSON_IsTrue(all))) {
        buf_append(&res, p, (size_t)(hit - p));
        buf_append(&res, nl, nlen);
        p = hit + olen;
        done_n++;
    }
    buf_puts(&res, p);
    if (plat_write_atomic(real, res.p ? res.p : "", res.len, 0) < 0) {
        put_errno(out, "写入", real);
        goto done;
    }

    char note[160];
    snprintf(note, sizeof note, "已修改 %s：替换了 %zu 处。修改后的片段：\n", real, done_n);
    buf_puts(out, note);
    size_t line = 1;
    for (size_t i = 0; i < first; i++)
        if (base[i] == '\n') line++;
    put_snippet(out, res.p ? res.p : "", res.len, line, count_lines(news, strlen(news)));
    ok = true;
done:
    buf_free(&text);
    buf_free(&res);
    buf_free(&needle);
    buf_free(&repl);
    free(real);
    return ok;
}
