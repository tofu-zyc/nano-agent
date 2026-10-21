/* nexec_main.c —— nano-exec 入口：读请求，准备目录和沙箱，执行工具，输出结果。见 nexec.h */
#include "nexec.h"
#include "sandbox.h"

#include <dirent.h>
#include <errno.h>
#include <pwd.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/utsname.h>
#include <unistd.h>

#define NEXEC_VERSION 1
#define REQ_MAX (64u << 20)

extern char **environ;

/* 沙箱里始终只读可见的系统路径（不存在的会被跳过） */
static const char *const SYSTEM_RO[] = {
    "/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt",
    "/proc", "/sys", "/nix/store", "/run/current-system", "/snap",
    "/dev/zero", "/dev/random", "/dev/urandom",
};
static const char *const SYSTEM_RW[] = { "/dev/null", "/dev/full" };

/* 从外面带进沙箱的环境变量；其余（比如 shell 配置里 export 的各种密钥）一律不带 */
static const char *const ENV_KEEP[] = { "PATH", "HOME", "USER", "LOGNAME", "LANG", "LANGUAGE", "TZ" };

typedef struct {
    const char *name;
    bool (*fn)(const xctx *, const cJSON *, buf_t *);
} tool_entry;

static const tool_entry TOOLS[] = {
    { "Read", tool_read }, { "Write", tool_write }, { "Edit", tool_edit },
    { "Grep", tool_grep }, { "Shell", tool_shell },
};

/* ---- 输出 ---- */

static void respond(bool ok, const char *content)
{
    cJSON *r = cJSON_CreateObject();
    cJSON_AddBoolToObject(r, "ok", ok);
    cJSON_AddStringToObject(r, "content", content);
    char *s = cJSON_PrintUnformatted(r);
    if (s) {
        fputs(s, stdout);
        fputc('\n', stdout);
        fflush(stdout);
    }
    cJSON_free(s);
    cJSON_Delete(r);
}

/* ---- 准备 ---- */

static void free_list(char **v, size_t n)
{
    for (size_t i = 0; i < n; i++) free(v[i]);
    free(v);
}

static void xctx_free(xctx *x)
{
    free_list(x->dirs, x->ndirs);
    free_list(x->ro, x->nro);
    free_list(x->rw, x->nrw);
    if (x->env) {
        for (char **e = x->env; *e; e++) free(*e);
        free(x->env);
    }
    free(x->home);
    free(x->tmp);
    free(x->cache);
    memset(x, 0, sizeof *x);
}

/* 读一行请求。内核只发这一行，之后 stdin 保持打开，关闭表示取消。 */
static char *read_request(void)
{
    buf_t b = {0};
    char tmp[65536];
    for (;;) {
        ssize_t n = read(0, tmp, sizeof tmp);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) break;
        buf_append(&b, tmp, (size_t)n);
        if (memchr(tmp, '\n', (size_t)n)) break;
        if (b.len > REQ_MAX) break;
    }
    if (!b.len || b.len > REQ_MAX) {
        buf_free(&b);
        return NULL;
    }
    return buf_cstr(&b);
}

static char *home_dir(void)
{
    const char *h = getenv("HOME");
    if (h && *h == '/') return xstrdup(h);
    struct passwd *pw = getpwuid(getuid());
    return xstrdup(pw && pw->pw_dir ? pw->pw_dir : "/");
}

static int make_dirs(const char *path)
{
    char *p = xstrdup(path);
    for (char *s = p + 1;; s++) {
        if (*s != '/' && *s) continue;
        char c = *s;
        *s = 0;
        if (mkdir(p, 0700) < 0 && errno != EEXIST) {
            int e = errno;
            free(p);
            errno = e;
            return -1;
        }
        if (!c) break;
        *s = c;
    }
    free(p);
    return 0;
}

static void push(char ***v, size_t *n, char *s)
{
    *v = xrealloc(*v, (*n + 2) * sizeof **v);
    (*v)[(*n)++] = s;
    (*v)[*n] = NULL;
}

/* 读字符串数组，展开 ~ 并解析符号链接。must_exist 时缺失的目录算错误，否则跳过。 */
static int load_paths(const xctx *x, const cJSON *arr, bool must_exist, char ***out, size_t *n, buf_t *err)
{
    const cJSON *it;
    cJSON_ArrayForEach(it, arr) {
        if (!cJSON_IsString(it) || !*it->valuestring) continue;
        char *p = expand_home(x->home, it->valuestring);
        char *real = p[0] == '/' ? realpath(p, NULL) : NULL;
        struct stat st;
        if (!real || (must_exist && (stat(real, &st) < 0 || !S_ISDIR(st.st_mode)))) {
            if (must_exist) {
                buf_puts(err, "工作目录不存在或不是目录：");
                buf_puts(err, it->valuestring);
                buf_puts(err, "。请让用户用 /dir 检查工作目录设置");
                free(p);
                free(real);
                return -1;
            }
            free(p);
            free(real);
            continue;
        }
        free(p);
        push(out, n, real);
    }
    return 0;
}

static bool keep_env(const char *e)
{
    if (!strncmp(e, "LC_", 3)) return true;
    for (size_t i = 0; i < sizeof ENV_KEEP / sizeof *ENV_KEEP; i++) {
        size_t l = strlen(ENV_KEEP[i]);
        if (!strncmp(e, ENV_KEEP[i], l) && e[l] == '=') return true;
    }
    return false;
}

static char **build_env(const xctx *x)
{
    char **env = NULL;
    size_t n = 0;
    bool has_path = false, has_lang = false;
    for (char **e = environ; *e; e++) {
        if (!keep_env(*e) || !strncmp(*e, "HOME=", 5)) continue;
        if (!strncmp(*e, "PATH=", 5)) has_path = true;
        if (!strncmp(*e, "LANG=", 5) || !strncmp(*e, "LC_ALL=", 7)) has_lang = true;
        push(&env, &n, xstrdup(*e));
    }
    if (!has_path) push(&env, &n, xstrdup("PATH=/usr/local/bin:/usr/bin:/bin"));
    if (!has_lang) push(&env, &n, xstrdup("LANG=C.UTF-8"));
    push(&env, &n, xasprintf("HOME=%s", x->home));
    push(&env, &n, xasprintf("TMPDIR=%s", x->tmp));
    push(&env, &n, xasprintf("XDG_CACHE_HOME=%s", x->cache));
    push(&env, &n, xstrdup("SHELL=/bin/sh"));
    push(&env, &n, xstrdup("TERM=dumb"));
    push(&env, &n, xstrdup("NO_COLOR=1"));
    push(&env, &n, xstrdup("PAGER=cat"));
    push(&env, &n, xstrdup("GIT_PAGER=cat"));
    push(&env, &n, xstrdup("GIT_TERMINAL_PROMPT=0"));
    return env;
}

/* tmp_key 只保留安全字符，用作目录名 */
static char *safe_key(const char *k)
{
    if (!k || !*k) return xstrdup("default");
    char *s = xstrdup(k);
    for (char *p = s; *p; p++)
        if (!((*p >= 'a' && *p <= 'z') || (*p >= 'A' && *p <= 'Z') || (*p >= '0' && *p <= '9') ||
              *p == '-' || *p == '_' || *p == '.'))
            *p = '_';
    if (s[0] == '.') s[0] = '_';
    return s;
}

static int setup(xctx *x, const cJSON *req, buf_t *err)
{
    memset(x, 0, sizeof *x);
    x->home = home_dir();
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(req, "max_output");
    x->max_output = cJSON_IsNumber(v) && v->valuedouble >= 1024 && v->valuedouble <= (64 << 20)
                  ? (size_t)v->valuedouble : 64 * 1024;
    x->write = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(req, "write"));
    x->watch_stdin = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(req, "watch_stdin"));

    if (load_paths(x, cJSON_GetObjectItemCaseSensitive(req, "dirs"), true, &x->dirs, &x->ndirs, err) < 0 ||
        load_paths(x, cJSON_GetObjectItemCaseSensitive(req, "ro"), false, &x->ro, &x->nro, err) < 0 ||
        load_paths(x, cJSON_GetObjectItemCaseSensitive(req, "rw"), false, &x->rw, &x->nrw, err) < 0)
        return -1;
    /* 可写的目录不能包含执行器自己：改写了它，下一次调用就不在沙箱里了。
     * 本机由内核把关；远程时执行器放在服务器的 ~/.cache 里，工作目录是 ~ 的话会被包含进来 */
    char *self = realpath("/proc/self/exe", NULL);
    for (size_t i = 0; self && i < x->ndirs + x->nrw; i++) {
        const char *d = i < x->ndirs ? x->dirs[i] : x->rw[i - x->ndirs];
        if ((i >= x->ndirs || x->write) && path_inside(self, d)) {
            buf_puts(err, "工作目录 ");
            buf_puts(err, d);
            buf_puts(err, " 包含执行器自己（");
            buf_puts(err, self);
            buf_puts(err, "），不能用作可写的工作目录。请让用户换一个更具体的目录");
            free(self);
            return -1;
        }
    }
    free(self);

    const char *xdg = getenv("XDG_CACHE_HOME");
    char *base = xdg && *xdg == '/' ? xasprintf("%s/nano-agent", xdg) : xasprintf("%s/.cache/nano-agent", x->home);
    char *key = safe_key(cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(req, "tmp_key")));
    x->tmp = xasprintf("%s/tmp/%s", base, key);
    x->cache = xasprintf("%s/cache", base);
    free(base);
    free(key);
    if (make_dirs(x->tmp) < 0 || make_dirs(x->cache) < 0) {
        buf_puts(err, "创建临时目录失败：");
        buf_puts(err, strerror(errno));
        return -1;
    }
    x->env = build_env(x);
    return 0;
}

/* PATH 里的目录（以及旁边的 lib，比如 nvm 装的 node）只读可见，用户自己装的命令才能用。
 * /mnt 下的不加：WSL 会把 Windows 的目录放进 PATH，那里的程序不受沙箱限制。 */
static void add_path_dirs(const xctx *x, char ***ro, size_t *nro)
{
    const char *path = NULL;
    for (char **e = x->env; *e; e++)
        if (!strncmp(*e, "PATH=", 5)) path = *e + 5;
    if (!path) return;
    char *dup = xstrdup(path), *save = NULL;
    for (char *d = strtok_r(dup, ":", &save); d; d = strtok_r(NULL, ":", &save)) {
        if (d[0] != '/' || !strcmp(d, "/") || path_inside(d, "/mnt")) continue;
        push(ro, nro, xstrdup(d));
        size_t l = strlen(d);
        if (l > 4 && !strcmp(d + l - 4, "/bin")) push(ro, nro, xasprintf("%.*s/lib", (int)(l - 4), d));
    }
    free(dup);
}

static int enter_sandbox(const xctx *x, buf_t *err)
{
    char **rw = NULL, **ro = NULL;
    size_t nrw = 0, nro = 0;
    for (size_t i = 0; i < x->ndirs; i++) push(x->write ? &rw : &ro, x->write ? &nrw : &nro, xstrdup(x->dirs[i]));
    for (size_t i = 0; i < x->nrw; i++) push(&rw, &nrw, xstrdup(x->rw[i]));
    for (size_t i = 0; i < x->nro; i++) push(&ro, &nro, xstrdup(x->ro[i]));
    push(&rw, &nrw, xstrdup(x->tmp));
    push(&rw, &nrw, xstrdup(x->cache));
    for (size_t i = 0; i < sizeof SYSTEM_RW / sizeof *SYSTEM_RW; i++) push(&rw, &nrw, xstrdup(SYSTEM_RW[i]));
    for (size_t i = 0; i < sizeof SYSTEM_RO / sizeof *SYSTEM_RO; i++) push(&ro, &nro, xstrdup(SYSTEM_RO[i]));
    add_path_dirs(x, &ro, &nro);
    /* /etc/resolv.conf 常常是指向 /run 或 /mnt/wsl 的链接，只放行它指向的那个文件 */
    char *resolv = realpath("/etc/resolv.conf", NULL);
    if (resolv) push(&ro, &nro, resolv);

    sb_rules r = { .rw = (const char *const *)rw, .nrw = nrw, .ro = (const char *const *)ro, .nro = nro };
    char msg[512];
    int rc = sb_apply(&r, msg, sizeof msg);
    if (rc < 0) {
        buf_puts(err, "沙箱启动失败，没有执行：");
        buf_puts(err, msg);
    }
    for (size_t i = 0; i < nrw; i++) free(rw[i]);
    for (size_t i = 0; i < nro; i++) free(ro[i]);
    free(rw);
    free(ro);
    return rc < 0 ? -1 : 0;
}

/* ---- 入口 ---- */

static int probe(void)
{
    struct utsname u;
    uname(&u);
    xctx x = { .env = environ };
    char *rg = find_in_path(&x, "rg");
    cJSON *r = cJSON_CreateObject();
    cJSON_AddNumberToObject(r, "v", NEXEC_VERSION);
    cJSON_AddNumberToObject(r, "landlock", sb_landlock_abi());
    cJSON_AddStringToObject(r, "os", u.sysname);
    cJSON_AddStringToObject(r, "arch", u.machine);
    cJSON_AddBoolToObject(r, "rg", rg != NULL);
    char *s = cJSON_PrintUnformatted(r);
    puts(s);
    cJSON_free(s);
    cJSON_Delete(r);
    free(rg);
    return 0;
}

/* --ls 路径：列出一个目录下的子目录（远程选工作目录用），格式和内核的 ls_result 一样。
 * 包含执行器自己的目录标成 blocked。只返回目录名，不读任何文件内容。 */
#define LS_MAX 1000

static int cmp_name(const void *a, const void *b)
{
    return strcmp(*(char *const *)a, *(char *const *)b);
}

static int list_dir(const char *arg)
{
    char *home = home_dir();
    char *p = expand_home(home, arg && *arg ? arg : "~");
    char *self = realpath("/proc/self/exe", NULL);
    cJSON *r = cJSON_CreateObject();
    cJSON_AddStringToObject(r, "home", home);
    char *real = p[0] == '/' ? realpath(p, NULL) : NULL;
    DIR *d = real ? opendir(real) : NULL;
    if (!d) {
        cJSON_AddBoolToObject(r, "ok", false);
        cJSON_AddStringToObject(r, "detail", p[0] != '/' ? "要写绝对路径或 ~ 开头" : strerror(errno));
    } else {
        cJSON_AddBoolToObject(r, "ok", true);
        cJSON_AddStringToObject(r, "path", real);
        cJSON_AddBoolToObject(r, "blocked", self && path_inside(self, real));
        char **names = NULL;
        size_t n = 0;
        bool more = false;
        struct dirent *e;
        while ((e = readdir(d))) {
            if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
            bool dir = e->d_type == DT_DIR;
            if (e->d_type == DT_UNKNOWN || e->d_type == DT_LNK) {
                struct stat st;
                dir = fstatat(dirfd(d), e->d_name, &st, 0) == 0 && S_ISDIR(st.st_mode);
            }
            if (!dir) continue;
            if (n >= LS_MAX) { more = true; continue; }
            names = xrealloc(names, (n + 1) * sizeof *names);
            names[n++] = xstrdup(e->d_name);
        }
        closedir(d);
        qsort(names, n, sizeof *names, cmp_name);
        cJSON *arr = cJSON_AddArrayToObject(r, "entries");
        for (size_t i = 0; i < n; i++) {
            cJSON *it = cJSON_CreateObject();
            buf_t b = {0};
            buf_put_utf8(&b, names[i], strlen(names[i]));
            cJSON_AddStringToObject(it, "name", buf_cstr(&b));
            buf_free(&b);
            char *sub = xasprintf("%s/%s", strcmp(real, "/") ? real : "", names[i]);
            if (self && path_inside(self, sub)) cJSON_AddBoolToObject(it, "blocked", true);
            free(sub);
            cJSON_AddItemToArray(arr, it);
            free(names[i]);
        }
        free(names);
        if (more) cJSON_AddBoolToObject(r, "truncated", true);
    }
    char *s = cJSON_PrintUnformatted(r);
    puts(s);
    cJSON_free(s);
    cJSON_Delete(r);
    free(real);
    free(p);
    free(self);
    free(home);
    return 0;
}

static void close_inherited_fds(void)
{
#ifdef SYS_close_range
    if (syscall(SYS_close_range, 3U, ~0U, 0U) == 0) return;
#endif
    for (int fd = 3; fd < 1024; fd++) close(fd);
}

int main(int argc, char **argv)
{
    if (argc > 1 && !strcmp(argv[1], "--probe")) return probe();
    if (argc > 1 && !strcmp(argv[1], "--ls")) return list_dir(argc > 2 ? argv[2] : "~");
    if (argc > 1) {
        fprintf(stderr, "用法：%s [--probe | --ls 目录]\n  从 stdin 读一行请求 JSON，向 stdout 写一行结果 JSON\n", argv[0]);
        return 2;
    }
    close_inherited_fds();
    signal(SIGPIPE, SIG_IGN);

    char *line = read_request();
    if (!line) {
        respond(false, "执行器没有收到请求");
        return 1;
    }
    cJSON *req = cJSON_Parse(line);
    free(line);
    if (!cJSON_IsObject(req)) {
        cJSON_Delete(req);
        respond(false, "执行器收到的请求不是合法的 JSON 对象");
        return 1;
    }

    const char *name = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(req, "tool"));
    const cJSON *args = cJSON_GetObjectItemCaseSensitive(req, "args");
    const tool_entry *t = NULL;
    for (size_t i = 0; name && i < sizeof TOOLS / sizeof *TOOLS; i++)
        if (!strcmp(TOOLS[i].name, name)) t = &TOOLS[i];

    buf_t out = {0};
    bool ok = false;
    xctx x = {0};
    if (!t) {
        buf_puts(&out, "未知工具：");
        buf_puts(&out, name ? name : "（空）");
    } else if (args && !cJSON_IsObject(args)) {
        buf_puts(&out, "参数应为 JSON 对象");
    } else if (setup(&x, req, &out) == 0 && enter_sandbox(&x, &out) == 0) {
        cJSON *empty = args ? NULL : cJSON_CreateObject();
        ok = t->fn(&x, args ? args : empty, &out);
        cJSON_Delete(empty);
    }
    if (!out.len) buf_puts(&out, "（无输出）");
    respond(ok, buf_cstr(&out));
    buf_free(&out);
    xctx_free(&x);
    cJSON_Delete(req);
    return 0;
}
