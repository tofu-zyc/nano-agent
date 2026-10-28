/* plat_posix.c —— 平台层的 POSIX 实现（Linux 和 macOS），见 plat.h */
#define _XOPEN_SOURCE 700   /* nftw */
#include "plat.h"

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <ftw.h>
#include <limits.h>
#include <netdb.h>
#include <netinet/in.h>
#include <poll.h>
#include <pwd.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#if !NA_MACOS
#include <sys/prctl.h>
#endif
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/un.h>
#include <sys/utsname.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#if NA_MACOS
#include <mach-o/dyld.h>
#endif

#define LINGER_MS 2000   /* 命令退出后，最多再等这么久让占着输出管道的后台进程放手 */

void plat_init(void) {}
void plat_shutdown(void) {}

char **plat_args(int argc, char **argv, int *n)
{
    *n = argc;
    return argv;
}

void plat_args_free(char **v, int n)
{
    (void)v;
    (void)n;
}

const char *plat_os(void)
{
#if NA_MACOS
    return "macos";
#else
    return "linux";
#endif
}

const char *plat_arch(void)
{
#if defined(__x86_64__)
    return "x86_64";
#elif defined(__aarch64__) || defined(__arm64__)
    return "arm64";
#elif defined(__riscv) && __riscv_xlen == 64
    return "riscv64";
#else
    return "unknown";
#endif
}

int64_t plat_now_ms(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

void plat_sleep_ms(int ms)
{
    struct timespec ts = { .tv_sec = ms / 1000, .tv_nsec = (long)(ms % 1000) * 1000000 };
    nanosleep(&ts, NULL);
}

char *plat_self_exe(void)
{
#if NA_MACOS
    uint32_t size = 0;
    _NSGetExecutablePath(NULL, &size);
    char *p = xmalloc(size);
    if (_NSGetExecutablePath(p, &size) != 0) { free(p); return NULL; }
    char *real = realpath(p, NULL);
    free(p);
    return real;
#else
    char self[PATH_MAX];
    ssize_t n = readlink("/proc/self/exe", self, sizeof self - 1);
    if (n <= 0) return NULL;
    self[n] = 0;
    return realpath(self, NULL);
#endif
}

int plat_exec(const char *path, char **argv)
{
    execv(path, argv);
    return -1;
}

char *plat_exe_dir(void)
{
    char *self = plat_self_exe();
    if (!self) return NULL;
    char *d = plat_dirname(self);
    free(self);
    return d;
}

char *plat_failure_detail(const char *stage, const char *path)
{
    int e = errno;
    return xasprintf("stage=%s path=%s errno=%d (%s)", stage, path, e, strerror(e));
}

char *plat_getenv_utf8(const char *name)
{
    const char *v = getenv(name);
    if (!v) { errno = ENOENT; return NULL; }
    return xstrdup(v);
}

char *plat_home(void)
{
    const char *h = getenv("HOME");
    if (h && *h == '/') return xstrdup(h);
    struct passwd *pw = getpwuid(getuid());
    if (pw && pw->pw_dir && *pw->pw_dir) return xstrdup(pw->pw_dir);
    char *self = plat_self_exe();
    char *d = self ? plat_dirname(self) : NULL;
    free(self);
    return d ? d : xstrdup("/");
}

char *plat_hostname(void)
{
    char host[256] = "kernel";
    gethostname(host, sizeof host - 1);
    host[sizeof host - 1] = 0;
    return xstrdup(host);
}

bool plat_is_abs(const char *p)
{
    return p && p[0] == '/';
}

bool plat_is_root(const char *p)
{
    return p && p[0] == '/' && p[1] == 0;
}

char *plat_expand_home(const char *p)
{
    if (p && p[0] == '~' && (p[1] == '/' || p[1] == 0)) {
        char *home = plat_home();
        char *out = xasprintf("%s%s", home, p + 1);
        free(home);
        return out;
    }
    return xstrdup(p ? p : "");
}

char *plat_dirname(const char *p)
{
    if (!p || !*p) return xstrdup(".");
    const char *slash = strrchr(p, '/');
    if (!slash) return xstrdup(".");
    if (slash == p) return xstrdup("/");
    char *d = xstrdup(p);
    d[slash - p] = 0;
    return d;
}

char *plat_join(const char *dir, const char *name)
{
    if (!dir || !*dir) return xstrdup(name ? name : "");
    if (!name || !*name) return xstrdup(dir);
    size_t l = strlen(dir);
    if (strcmp(dir, "/") == 0) return xasprintf("/%s", name);
    if (dir[l - 1] == '/') return xasprintf("%s%s", dir, name);
    return xasprintf("%s/%s", dir, name);
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
            if (len > 0) len--;
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

char *plat_abspath(const char *base, const char *p)
{
    char *full;
    if (!p || !*p) full = xstrdup(base && *base ? base : "/");
    else if (p[0] == '/') full = xstrdup(p);
    else {
        char cwd[PATH_MAX];
        const char *b = base && *base ? base : getcwd(cwd, sizeof cwd);
        if (!b) return NULL;
        full = xasprintf("%s/%s", b, p);
    }
    char *abs = normalize(full);
    free(full);
    return abs;
}

char *plat_canonical(const char *abs)
{
    if (!abs || !*abs) return NULL;
    char *p = xstrdup(abs);
    size_t cut = strlen(p);
    for (;;) {
        char saved = p[cut];
        p[cut] = 0;
        char *real = realpath(cut ? p : "/", NULL);
        p[cut] = saved;
        if (real) {
            char *res;
            if (!p[cut]) res = xstrdup(real);
            else if (strcmp(real, "/") == 0) res = xstrdup(p + cut);
            else res = xasprintf("%s%s", real, p + cut);
            free(real);
            free(p);
            return res;
        }
        if (errno != ENOENT && errno != ENOTDIR) {
            free(p);
            return NULL;
        }
        while (cut > 0 && p[cut - 1] != '/') cut--;
        if (cut > 0) cut--;
        if (cut == 0) {
            /* 连根都不存在（不可能），按字面返回 */
            char *res = normalize(abs);
            free(p);
            return res;
        }
    }
}

bool plat_inside(const char *path, const char *dir)
{
    if (!path || !dir) return false;
    size_t n = strlen(dir);
    if (n == 1 && dir[0] == '/') return true;
    if (strncmp(path, dir, n) != 0) return false;
    return path[n] == 0 || path[n] == '/';
}

bool plat_path_eq(const char *a, const char *b)
{
    return a && b && strcmp(a, b) == 0;
}

const char *plat_sep(void) { return "/"; }

bool plat_stat(const char *p, plat_stat_t *st)
{
    struct stat s;
    memset(st, 0, sizeof *st);
    if (stat(p, &s) < 0) return false;
    st->exists = true;
    st->is_dir = S_ISDIR(s.st_mode);
    st->size = (int64_t)s.st_size;
    return true;
}

bool plat_is_executable(const char *p)
{
    return access(p, X_OK) == 0;
}

static int make_dirs_mode(const char *path)
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

int plat_makedirs(const char *p)
{
    return make_dirs_mode(p);
}

int plat_mkdir(const char *p)
{
    return mkdir(p, 0700);
}

static int rm_entry(const char *path, const struct stat *st, int flag, struct FTW *ftw)
{
    (void)st; (void)flag; (void)ftw;
    return remove(path);
}

int plat_remove_tree(const char *p)
{
    if (!p || !*p) return 0;
    if (nftw(p, rm_entry, 16, FTW_DEPTH | FTW_PHYS) == 0) return 0;
    return errno == ENOENT ? 0 : -1;
}

int plat_rename(const char *from, const char *to)
{
    return rename(from, to);
}

int plat_unlink(const char *p)
{
    return unlink(p);
}

int plat_write_atomic(const char *path, const char *data, size_t n, int mode)
{
    struct stat st;
    bool exists = stat(path, &st) == 0;
    const char *slash = strrchr(path, '/');
    if (!slash) { errno = EINVAL; return -1; }
    char *tmp = xasprintf("%.*s/.%s.nano-XXXXXX", (int)(slash - path), path, slash + 1);
    int fd = mkstemp(tmp);
    if (fd < 0) { free(tmp); return -1; }
    int perm;
    if (exists) {
        perm = st.st_mode & 07777;
    } else if (mode) {
        perm = mode & 07777;
    } else {
        mode_t um = umask(0);
        umask(um);
        perm = 0666 & ~um;
    }
    size_t off = 0;
    int rc = 0;
    while (off < n) {
        ssize_t w = write(fd, data + off, n - off);
        if (w < 0 && errno == EINTR) continue;
        if (w < 0) { rc = -1; break; }
        off += (size_t)w;
    }
    if (rc == 0 && fchmod(fd, (mode_t)perm) < 0) rc = -1;
    int saved = errno;
    if (close(fd) < 0 && rc == 0) { rc = -1; saved = errno; }
    if (rc == 0 && rename(tmp, path) < 0) { rc = -1; saved = errno; }
    if (rc) unlink(tmp);
    free(tmp);
    errno = saved;
    return rc;
}

int plat_read_file(const char *path, buf_t *out, size_t max)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return -1;
    char tmp[65536];
    for (;;) {
        ssize_t n = read(fd, tmp, sizeof tmp);
        if (n < 0 && errno == EINTR) continue;
        if (n < 0) { int e = errno; close(fd); errno = e; return -1; }
        if (n == 0) break;
        if (out->len + (size_t)n > max) { close(fd); errno = EFBIG; return -1; }
        buf_append(out, tmp, (size_t)n);
    }
    close(fd);
    return 0;
}

bool plat_read_dir(const char *path, size_t max, plat_dirent **ents, size_t *n, bool *truncated,
                   char *err, size_t errlen)
{
    *ents = NULL;
    *n = 0;
    *truncated = false;
    DIR *d = opendir(path);
    if (!d) {
        if (err) snprintf(err, errlen, "打开目录失败：%s", strerror(errno));
        return false;
    }
    struct dirent *e;
    while ((e = readdir(d))) {
        if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
        bool dir = e->d_type == DT_DIR;
        if (e->d_type == DT_UNKNOWN || e->d_type == DT_LNK) {
            struct stat st;
            dir = fstatat(dirfd(d), e->d_name, &st, 0) == 0 && S_ISDIR(st.st_mode);
        }
        if (*n >= max) { *truncated = true; continue; }
        *ents = xrealloc(*ents, (*n + 1) * sizeof **ents);
        (*ents)[*n].name = xstrdup(e->d_name);
        (*ents)[*n].dir = dir;
        (*n)++;
    }
    closedir(d);
    return true;
}

void plat_dirents_free(plat_dirent *ents, size_t n)
{
    for (size_t i = 0; i < n; i++) free(ents[i].name);
    free(ents);
}

char **plat_drives(size_t *n)
{
    *n = 0;
    return NULL;
}

void plat_drives_free(char **v, size_t n)
{
    for (size_t i = 0; i < n; i++) free(v[i]);
    free(v);
}

struct plat_file {
    int fd;
};

plat_file *plat_open_read(const char *path)
{
    int fd = open(path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return NULL;
    plat_file *f = xmalloc(sizeof *f);
    f->fd = fd;
    return f;
}

long plat_file_read(plat_file *f, void *buf, size_t n)
{
    for (;;) {
        ssize_t r = read(f->fd, buf, n);
        if (r < 0 && errno == EINTR) continue;
        return (long)r;
    }
}

void plat_file_close(plat_file *f)
{
    if (!f) return;
    close(f->fd);
    free(f);
}

char *plat_relpath(const char *path, const char *base)
{
    if (!plat_inside(path, base)) return NULL;
    size_t n = strlen(base);
    if (n == 1 && base[0] == '/') return xstrdup(path[0] == '/' ? path + 1 : path);
    if (!path[n]) return xstrdup(".");
    return xstrdup(path + n + 1);
}

/* ---- 子进程 ---- */

static int g_sig[2] = { -1, -1 };

static void on_sig(int sig)
{
    int saved = errno;
    char c = (char)sig;
    if (write(g_sig[1], &c, 1) < 0) { /* 管道满了也无妨 */ }
    errno = saved;
}

typedef struct {
    buf_t  head, tail;
    size_t head_max, tail_max, total;
} capture;

static void cap_add(capture *c, const char *p, size_t n)
{
    c->total += n;
    if (c->head.len < c->head_max) {
        size_t take = c->head_max - c->head.len;
        if (take > n) take = n;
        buf_append(&c->head, p, take);
        p += take;
        n -= take;
    }
    if (!n) return;
    buf_append(&c->tail, p, n);
    if (c->tail.len > 2 * c->tail_max) buf_consume(&c->tail, c->tail.len - c->tail_max);
}

static void cap_render(capture *c, buf_t *out)
{
    const char *h = c->head.p ? c->head.p : "";
    size_t skip = c->tail.len > c->tail_max ? c->tail.len - c->tail_max : 0;
    const char *t = c->tail.p ? c->tail.p + skip : "";
    size_t tl = c->tail.len - skip;
    size_t omitted = c->total - c->head.len - tl;
    if (!omitted) {
        buf_t all = {0};
        buf_append(&all, h, c->head.len);
        buf_append(&all, t, tl);
        buf_put_utf8(out, buf_cstr(&all), all.len);
        buf_free(&all);
        return;
    }
    buf_put_utf8(out, h, utf8_cut(h, c->head.len, c->head.len));
    while (tl && ((unsigned char)*t & 0xC0) == 0x80) { t++; tl--; }
    char note[96];
    snprintf(note, sizeof note, "\n…（中间省略 %zu 字节）…\n", omitted);
    buf_puts(out, note);
    buf_put_utf8(out, t, tl);
}

char *plat_find_in_path(char *const envp[], const char *name)
{
    if (!name || !*name) return NULL;
    if (strchr(name, '/')) {
        if (access(name, X_OK) == 0) return xstrdup(name);
        return NULL;
    }
    const char *path = NULL;
    for (char *const *e = envp; e && *e; e++)
        if (!strncmp(*e, "PATH=", 5)) path = *e + 5;
    if (!path) path = "/usr/local/bin:/usr/bin:/bin";
    char *dup = xstrdup(path), *save = NULL;
    for (char *d = strtok_r(dup, ":", &save); d; d = strtok_r(NULL, ":", &save)) {
        char *full = xasprintf("%s/%s", d, name);
        if (access(full, X_OK) == 0) {
            free(dup);
            return full;
        }
        free(full);
    }
    free(dup);
    return NULL;
}

/* 从外面带进沙箱的环境变量；其余（比如 shell 配置里 export 的各种密钥）一律不带 */
static const char *const ENV_KEEP[] = { "PATH", "HOME", "USER", "LOGNAME", "LANG", "LANGUAGE", "TZ", "NANO_HOME" };

static void env_push(char ***v, size_t *n, char *s)
{
    *v = xrealloc(*v, (*n + 2) * sizeof **v);
    (*v)[(*n)++] = s;
    (*v)[*n] = NULL;
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

char **plat_build_env(const char *home, const char *tmp, const char *cache)
{
    extern char **environ;
    char **env = NULL;
    size_t n = 0;
    bool has_path = false, has_lang = false;
    for (char **e = environ; *e; e++) {
        if (!keep_env(*e) || !strncmp(*e, "HOME=", 5)) continue;
        if (!strncmp(*e, "PATH=", 5)) has_path = true;
        if (!strncmp(*e, "LANG=", 5) || !strncmp(*e, "LC_ALL=", 7)) has_lang = true;
        env_push(&env, &n, xstrdup(*e));
    }
    if (!has_path) env_push(&env, &n, xstrdup("PATH=/usr/local/bin:/usr/bin:/bin"));
    if (!has_lang) env_push(&env, &n, xstrdup("LANG=C.UTF-8"));
    env_push(&env, &n, xasprintf("HOME=%s", home));
    env_push(&env, &n, xasprintf("TMPDIR=%s", tmp));
    env_push(&env, &n, xasprintf("XDG_CACHE_HOME=%s", cache));
    env_push(&env, &n, xstrdup("SHELL=/bin/sh"));
    env_push(&env, &n, xstrdup("TERM=dumb"));
    env_push(&env, &n, xstrdup("NO_COLOR=1"));
    env_push(&env, &n, xstrdup("PAGER=cat"));
    env_push(&env, &n, xstrdup("GIT_PAGER=cat"));
    env_push(&env, &n, xstrdup("GIT_TERMINAL_PROMPT=0"));
    return env;
}

int plat_run_proc(char *const argv[], const char *cwd, char *const envp[], int timeout_s,
                  bool watch_stdin, size_t max_output, buf_t *out, proc_status *st)
{
    memset(st, 0, sizeof *st);
    int p[2];
    if (pipe(p) < 0) {
        buf_puts(out, "创建管道失败：");
        buf_puts(out, strerror(errno));
        return -1;
    }
    set_cloexec(p[0]);
    set_cloexec(p[1]);
    if (g_sig[0] < 0) {
        if (pipe(g_sig) < 0) { close(p[0]); close(p[1]); return -1; }
        set_nonblock(g_sig[0]); set_nonblock(g_sig[1]);
        set_cloexec(g_sig[0]); set_cloexec(g_sig[1]);
        struct sigaction sa;
        memset(&sa, 0, sizeof sa);
        sa.sa_handler = on_sig;
        sigemptyset(&sa.sa_mask);
        sa.sa_flags = SA_RESTART | SA_NOCLDSTOP;
        sigaction(SIGCHLD, &sa, NULL);
        sigaction(SIGTERM, &sa, NULL);
        sigaction(SIGHUP, &sa, NULL);
    }

    pid_t parent = getpid();
    pid_t pid = fork();
    if (pid < 0) {
        buf_puts(out, "fork 失败：");
        buf_puts(out, strerror(errno));
        close(p[0]); close(p[1]);
        return -1;
    }
    if (pid == 0) {
        setpgid(0, 0);
#if !NA_MACOS
        prctl(PR_SET_PDEATHSIG, SIGKILL);
#endif
        if (getppid() != parent) _exit(127);
        signal(SIGCHLD, SIG_DFL);
        signal(SIGTERM, SIG_DFL);
        signal(SIGHUP, SIG_DFL);
        signal(SIGPIPE, SIG_DFL);
        int dn = open("/dev/null", O_RDONLY);
        if (dn < 0 || dup2(dn, 0) < 0 || dup2(p[1], 1) < 0 || dup2(p[1], 2) < 0) _exit(127);
        if (chdir(cwd) < 0) {
            dprintf(2, "进入目录 %s 失败：%s\n", cwd, strerror(errno));
            _exit(127);
        }
        execve(argv[0], argv, envp);
        dprintf(2, "无法执行 %s：%s\n", argv[0], strerror(errno));
        _exit(127);
    }
    setpgid(pid, pid);
    close(p[1]);
    set_nonblock(p[0]);

    capture c = { .head_max = max_output * 3 / 4, .tail_max = max_output / 4 };
    int64_t deadline = now_ms() + (int64_t)timeout_s * 1000, linger = -1;
    bool exited = false, open_out = true, cancelled = false;
    char tmp[16384];

    while (open_out || !exited) {
        int64_t now = now_ms();
        if (!exited && now >= deadline) {
            st->timed_out = true;
            break;
        }
        if (exited && now >= linger) break;
        int64_t until = exited ? linger : deadline;
        struct pollfd pf[3] = {
            { .fd = g_sig[0], .events = POLLIN },
            { .fd = open_out ? p[0] : -1, .events = POLLIN },
            { .fd = watch_stdin ? 0 : -1, .events = POLLIN },
        };
        int wait = (int)(until - now);
        if (wait > 1000) wait = 1000;
        if (poll(pf, 3, wait) < 0 && errno != EINTR) break;

        if (pf[0].revents & POLLIN) {
            char s[16];
            ssize_t n;
            while ((n = read(g_sig[0], s, sizeof s)) > 0)
                for (ssize_t i = 0; i < n; i++)
                    if (s[i] == SIGTERM || s[i] == SIGHUP) cancelled = true;
        }
        if (pf[2].revents & (POLLIN | POLLHUP | POLLERR)) {
            ssize_t n = read(0, tmp, sizeof tmp);
            if (n <= 0 && !(n < 0 && (errno == EINTR || errno == EAGAIN))) cancelled = true;
        }
        if (cancelled) {
            kill(-pid, SIGKILL);
            _exit(2);
        }
        if (pf[1].revents & (POLLIN | POLLHUP | POLLERR)) {
            for (int i = 0; i < 16; i++) {
                ssize_t n = read(p[0], tmp, sizeof tmp);
                if (n > 0) { cap_add(&c, tmp, (size_t)n); continue; }
                if (n < 0 && errno == EINTR) continue;
                if (n < 0 && errno == EAGAIN) break;
                open_out = false;
                break;
            }
        }
        int ws;
        if (!exited && waitpid(pid, &ws, WNOHANG) == pid) {
            exited = true;
            st->exit_code = WIFEXITED(ws) ? WEXITSTATUS(ws) : 128 + WTERMSIG(ws);
            st->term_signal = WIFSIGNALED(ws) ? WTERMSIG(ws) : 0;
            linger = now_ms() + LINGER_MS;
        }
    }

    kill(-pid, SIGKILL);
    if (!exited) {
        int ws;
        if (waitpid(pid, &ws, 0) == pid) {
            st->exit_code = WIFEXITED(ws) ? WEXITSTATUS(ws) : 128 + WTERMSIG(ws);
            st->term_signal = WIFSIGNALED(ws) ? WTERMSIG(ws) : 0;
        }
    }
    close(p[0]);
    cap_render(&c, out);
    buf_free(&c.head);
    buf_free(&c.tail);
    return 0;
}

int plat_probe(char *const argv[], int timeout_ms, buf_t *out)
{
    int p[2];
    if (pipe(p) < 0) return -1;
    pid_t pid = fork();
    if (pid < 0) { close(p[0]); close(p[1]); return -1; }
    if (pid == 0) {
        dup2(p[1], 1);
        close(p[0]);
        close(p[1]);
        execv(argv[0], argv);
        _exit(127);
    }
    close(p[1]);
    char tmp[4096];
    int64_t deadline = now_ms() + timeout_ms;
    for (;;) {
        struct pollfd pf = { .fd = p[0], .events = POLLIN };
        int64_t left = deadline - now_ms();
        if (left <= 0 || poll(&pf, 1, (int)left) <= 0) break;
        ssize_t n = read(p[0], tmp, sizeof tmp);
        if (n <= 0) break;
        buf_append(out, tmp, (size_t)n);
    }
    close(p[0]);
    kill(pid, SIGKILL);
    waitpid(pid, NULL, 0);
    return out->len ? 0 : -1;
}

char *plat_shell_exe(char *const envp[])
{
    (void)envp;
    return xstrdup("/bin/sh");
}

const char *plat_shell_prefix(void)
{
    return NULL;
}

/* ---- 网络、poll、唤醒 ---- */

int plat_poll(na_pollfd *fds, size_t n, int timeout_ms)
{
    struct pollfd *p = xcalloc(n ? n : 1, sizeof *p);
    for (size_t i = 0; i < n; i++) {
        p[i].fd = (int)fds[i].id;
        short e = 0;
        if (fds[i].events & NA_POLLIN) e |= POLLIN;
        if (fds[i].events & NA_POLLOUT) e |= POLLOUT;
        p[i].events = e;
    }
    int rc = poll(p, (nfds_t)n, timeout_ms);
    if (rc >= 0)
        for (size_t i = 0; i < n; i++) {
            short e = 0;
            if (p[i].revents & POLLIN) e |= NA_POLLIN;
            if (p[i].revents & POLLOUT) e |= NA_POLLOUT;
            if (p[i].revents & POLLERR) e |= NA_POLLERR;
            if (p[i].revents & POLLHUP) e |= NA_POLLHUP;
            if (p[i].revents & POLLNVAL) e |= NA_POLLNVAL;
            fds[i].revents = e;
        }
    free(p);
    return rc;
}

long plat_sock_read(na_sock s, void *buf, size_t n)
{
    for (;;) {
        ssize_t r = read((int)s, buf, n);
        if (r < 0 && errno == EINTR) continue;
        return (long)r;
    }
}

long plat_sock_write(na_sock s, const void *buf, size_t n)
{
    for (;;) {
        ssize_t r = write((int)s, buf, n);
        if (r < 0 && errno == EINTR) continue;
        return (long)r;
    }
}

void plat_sock_close(na_sock s)
{
    if (s != NA_SOCK_NONE) close((int)s);
}

static int connect_wait(int fd, const struct sockaddr *sa, socklen_t len, int timeout_ms)
{
    if (set_nonblock(fd) < 0) return -1;
    if (connect(fd, sa, len) == 0) return 0;
    if (errno != EINPROGRESS) return -1;
    struct pollfd p = { .fd = fd, .events = POLLOUT };
    int n = poll(&p, 1, timeout_ms);
    if (n == 0) { errno = ETIMEDOUT; return -1; }
    if (n < 0) return -1;
    int soerr = 0;
    socklen_t sl = sizeof soerr;
    if (getsockopt(fd, SOL_SOCKET, SO_ERROR, &soerr, &sl) < 0) return -1;
    if (soerr) { errno = soerr; return -1; }
    return 0;
}

static int connect_unix(const char *path, int timeout_ms, char *err, size_t errlen)
{
    struct sockaddr_un sa = { .sun_family = AF_UNIX };
    if (strlen(path) >= sizeof sa.sun_path) {
        snprintf(err, errlen, "socket 路径太长：%s", path);
        return -1;
    }
    strcpy(sa.sun_path, path);
    int fd = socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0 || connect_wait(fd, (struct sockaddr *)&sa, sizeof sa, timeout_ms) < 0) {
        snprintf(err, errlen, "连接 %s 失败：%s", path, strerror(errno));
        if (fd >= 0) close(fd);
        return -1;
    }
    return fd;
}

static int connect_tcp(const char *hostport, int timeout_ms, char *err, size_t errlen)
{
    char host[256];
    const char *port;
    if (hostport[0] == '[') {
        const char *rb = strchr(hostport, ']');
        if (!rb || rb[1] != ':' || (size_t)(rb - hostport - 1) >= sizeof host) goto bad;
        memcpy(host, hostport + 1, (size_t)(rb - hostport - 1));
        host[rb - hostport - 1] = 0;
        port = rb + 2;
    } else {
        const char *colon = strrchr(hostport, ':');
        if (!colon || (size_t)(colon - hostport) >= sizeof host) goto bad;
        memcpy(host, hostport, (size_t)(colon - hostport));
        host[colon - hostport] = 0;
        port = colon + 1;
    }

    struct addrinfo hints = { .ai_family = AF_UNSPEC, .ai_socktype = SOCK_STREAM }, *res;
    int gai = getaddrinfo(host, port, &hints, &res);
    if (gai) {
        snprintf(err, errlen, "解析 %s 失败：%s", hostport, gai_strerror(gai));
        return -1;
    }
    int fd = -1, last = 0;
    for (struct addrinfo *ai = res; ai; ai = ai->ai_next) {
        fd = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
        if (fd < 0) { last = errno; continue; }
        if (connect_wait(fd, ai->ai_addr, ai->ai_addrlen, timeout_ms) == 0) break;
        last = errno;
        close(fd);
        fd = -1;
    }
    freeaddrinfo(res);
    if (fd < 0) {
        snprintf(err, errlen, "连接 %s 失败：%s", hostport, strerror(last));
        return -1;
    }
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &one, sizeof one);
    return fd;

bad:
    snprintf(err, errlen, "地址格式不对：%s（应为 host:port）", hostport);
    return -1;
}

int plat_connect(const char *addr, int timeout_ms, na_sock *out, char *err, size_t errlen)
{
    int fd;
    if (!strncmp(addr, "unix:", 5)) fd = connect_unix(addr + 5, timeout_ms, err, errlen);
    else if (!strncmp(addr, "tcp:", 4)) fd = connect_tcp(addr + 4, timeout_ms, err, errlen);
    else fd = connect_tcp(addr, timeout_ms, err, errlen);
    if (fd < 0) return -1;
    set_nonblock(fd);
    set_cloexec(fd);
    *out = (na_sock)fd;
    return 0;
}

int plat_wake_open(na_wake *w)
{
    int p[2];
    if (pipe(p) < 0) return -1;
    set_nonblock(p[0]);
    set_nonblock(p[1]);
    set_cloexec(p[0]);
    set_cloexec(p[1]);
    w->rd = (intptr_t)p[0];
    w->wr = (intptr_t)p[1];
    return 0;
}

void plat_wake_signal(na_wake *w)
{
    if (w->wr < 0) return;
    char c = 1;
    if (write((int)w->wr, &c, 1) < 0) { /* 满了也无妨 */ }
}

void plat_wake_drain(na_wake *w)
{
    char buf[64];
    while (read((int)w->rd, buf, sizeof buf) > 0) {}
}

void plat_wake_close(na_wake *w)
{
    if (w->rd >= 0) close((int)w->rd);
    if (w->wr >= 0) close((int)w->wr);
    w->rd = w->wr = -1;
}

static na_wake *g_swake;
static volatile int *g_sstop;

static void on_stop_signal(int sig)
{
    int saved = errno;
    if (g_sstop && (sig == SIGINT || sig == SIGTERM || sig == SIGHUP)) *g_sstop = 1;
    if (g_swake) plat_wake_signal(g_swake);
    errno = saved;
}

int plat_signals_install(na_wake *w, volatile int *stop)
{
    g_swake = w;
    g_sstop = stop;
    struct sigaction sa;
    memset(&sa, 0, sizeof sa);
    sa.sa_handler = on_stop_signal;
    sigemptyset(&sa.sa_mask);
    sa.sa_flags = SA_RESTART | SA_NOCLDSTOP;
    sigaction(SIGINT, &sa, NULL);
    sigaction(SIGTERM, &sa, NULL);
    sigaction(SIGHUP, &sa, NULL);
    sigaction(SIGCHLD, &sa, NULL);
    signal(SIGPIPE, SIG_IGN);
    return 0;
}

void plat_signals_remove(void)
{
    signal(SIGINT, SIG_DFL);
    signal(SIGTERM, SIG_DFL);
    signal(SIGHUP, SIG_DFL);
    signal(SIGCHLD, SIG_DFL);
}

/* ---- 管理命令用的进程操作 ---- */

long plat_getpid(void) { return (long)getpid(); }

int plat_log_to(const char *path)
{
    int fd = open(path, O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0600);
    if (fd < 0) return -1;
    if (fd != 2) {
        dup2(fd, 2);
        close(fd);
    }
    return 0;
}

bool plat_pid_alive(long pid, const char *name)
{
    if (pid <= 0) return false;
    if (kill((pid_t)pid, 0) < 0 && errno != EPERM) return false;
#ifdef __linux__
    if (name) {
        char link[64], buf[PATH_MAX];
        snprintf(link, sizeof link, "/proc/%ld/exe", pid);
        ssize_t n = readlink(link, buf, sizeof buf - 1);
        if (n > 0) {
            buf[n] = 0;
            const char *base = strrchr(buf, '/');
            return strstr(base ? base + 1 : buf, name) != NULL;
        }
    }
#else
    (void)name;
#endif
    return true;
}

int plat_pid_kill(long pid, bool force)
{
    return kill((pid_t)pid, force ? SIGKILL : SIGTERM);
}

int plat_system(char *const argv[], bool quiet)
{
    pid_t pid = fork();
    if (pid < 0) return -1;
    if (pid == 0) {
        if (quiet) {
            int fd = open("/dev/null", O_RDWR);
            if (fd >= 0) {
                dup2(fd, 1);
                dup2(fd, 2);
                if (fd > 2) close(fd);
            }
        }
        execvp(argv[0], argv);
        _exit(127);
    }
    int st;
    while (waitpid(pid, &st, 0) < 0)
        if (errno != EINTR) return -1;
    if (WIFEXITED(st)) return WEXITSTATUS(st) == 127 ? -1 : WEXITSTATUS(st);
    return -1;
}

long plat_spawn_bg(char *const argv[], const char *log)
{
    pid_t pid = fork();
    if (pid < 0) return -1;
    if (pid == 0) {
        setsid();
        int nul = open("/dev/null", O_RDWR);
        int err = log ? open(log, O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0600) : -1;
        if (nul >= 0) {
            dup2(nul, 0);
            dup2(nul, 1);
            dup2(err >= 0 ? err : nul, 2);
        }
        execv(argv[0], argv);
        _exit(127);
    }
    return (long)pid;
}
