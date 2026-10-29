/* plat.h —— 各平台不同的小平台层：路径、文件、自己、子进程
 *
 * 上层（执行器、内核）只认 UTF-8 字符串和这里定义的接口；Windows 上的宽字符转换
 * 都留在 plat_win.c 里。POSIX 一份（Linux 和 macOS 共用），Windows 一份。
 */
#ifndef NA_PLAT_H
#define NA_PLAT_H

#include "util.h"

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#if defined(_WIN32)
#define NA_WINDOWS 1
#else
#define NA_WINDOWS 0
#endif
#if defined(__APPLE__)
#define NA_MACOS 1
#else
#define NA_MACOS 0
#endif

/* 进程启动时调用一次：Windows 上初始化 Winsock、把标准输入输出设成二进制/UTF-8 */
void plat_init(void);
void plat_shutdown(void);
#if NA_WINDOWS
/* Shared Windows argv quoting for both the executor and its supervisor. */
wchar_t *plat_command_line(char *const argv[]);
#endif

/* main 收到的参数。Windows 的 argv 是 ANSI 的，中文路径会被切坏；从 GetCommandLineW 重新转成 UTF-8。 */
char **plat_args(int argc, char **argv, int *n);
void   plat_args_free(char **v, int n);

const char *plat_os(void);     /* linux / macos / windows */
const char *plat_arch(void);   /* x86_64 / arm64 / riscv64 */
int64_t plat_now_ms(void);
int plat_random_bytes(void *out, size_t n);
void plat_sleep_ms(int ms);

/* 自己、主目录、主机名。返回 malloc 的 UTF-8 字符串。 */
char *plat_self_exe(void);
char *plat_exe_dir(void);
char *plat_home(void);
/* malloc UTF-8 value, including an allocated empty string. NULL sets errno:
 * ENOENT means absent; other errors must not silently select a different root. */
char *plat_getenv_utf8(const char *name);
/* Bounded diagnostic after a filesystem failure; never changes token policy. */
char *plat_failure_detail(const char *stage, const char *path);
char *plat_hostname(void);
/* 用 argv 执行 path，替换当前进程（更新后重启自己）。POSIX 上成功不返回；Windows 上返回 -1、errno = ENOSYS。 */
int plat_exec(const char *path, char **argv);

/* ---- 路径（全部是 UTF-8；返回 malloc） ---- */

bool  plat_is_abs(const char *p);
bool  plat_is_root(const char *p);
char *plat_expand_home(const char *p);                 /* ~ 开头换成主目录，其他原样复制 */
char *plat_abspath(const char *base, const char *p);   /* 绝对化，合并 . .. 和多余的分隔符 */
/* 解析符号链接 / junction / 盘符大小写，变成那台机器上的规范形式。
 * 文件可以不存在：解析最近的已存在上级，再接上剩下的部分。失败返回 NULL。 */
char *plat_canonical(const char *abs);
char *plat_dirname(const char *p);
char *plat_join(const char *dir, const char *name);
char *plat_relpath(const char *path, const char *base); /* 不在 base 下返回 NULL */
bool  plat_inside(const char *path, const char *dir);
bool  plat_path_eq(const char *a, const char *b);       /* Windows 上不区分大小写 */
const char *plat_sep(void);                             /* "\\" 或 "/" */

/* ---- 文件 ---- */

typedef struct {
    bool    exists;
    bool    is_dir;
    int64_t size;
} plat_stat_t;

bool plat_stat(const char *p, plat_stat_t *st);
bool plat_is_executable(const char *p);     /* 存在且能执行（Windows 上只要求是文件） */
int  plat_makedirs(const char *p);          /* mkdir -p，0700 */
int  plat_mkdir(const char *p);
int  plat_remove_tree(const char *p);       /* 递归删除，不存在不算错 */
int  plat_rename(const char *from, const char *to);
int  plat_unlink(const char *p);
/* 原子写：同目录临时文件再改名。mode 是新建文件的权限（已有文件保留原权限）。 */
int  plat_write_atomic(const char *path, const char *data, size_t n, int mode);
/* 读整个文件，最多 max 字节；超出时 errno = EFBIG。 */
int  plat_read_file(const char *path, buf_t *out, size_t max);
/* Job persistence: append logs, flush a log fence, and durably publish metadata. */
int plat_append_file(const char *path,const void *data,size_t n,bool sync);
int plat_sync_file(const char *path);
int plat_write_durable(const char *path,const char *data,size_t n,int mode);


/* ---- 目录 ---- */

typedef struct {
    char *name;   /* 子项名字（不含目录） */
    bool  dir;
} plat_dirent;

/* 列出目录内容。max 是名字数量上限，超出时 *truncated = true。 */
bool plat_read_dir(const char *path, size_t max, plat_dirent **ents, size_t *n, bool *truncated,
                   char *err, size_t errlen);
void plat_dirents_free(plat_dirent *ents, size_t n);
/* Windows 上返回盘符（"C:\\" 这种完整根路径）；其他平台返回 NULL、*n = 0。 */
char **plat_drives(size_t *n);
void plat_drives_free(char **v, size_t n);

/* ---- 读文件（Read 工具按行读，不整个载入） ---- */

typedef struct plat_file plat_file;
plat_file *plat_open_read(const char *path);
long       plat_file_read(plat_file *f, void *buf, size_t n);   /* 0 = EOF，-1 = 出错 */
void       plat_file_close(plat_file *f);

/* ---- 子进程（执行器） ---- */

typedef struct {
    int  exit_code;    /* 正常退出时的退出码；被信号结束时无意义 */
    int  term_signal;  /* 被信号结束时的信号编号，0 表示正常退出 */
    bool timed_out;
    bool cancelled;
} proc_status;

char *plat_find_in_path(char *const envp[], const char *name);

/* 执行器子进程的环境变量（以 NULL 结尾）：只带必要的系统变量，HOME/TMPDIR 等换成会话的目录 */
char **plat_build_env(const char *home, const char *tmp, const char *cache);

/* 运行 argv（stdout 和 stderr 合在一起），输出保留开头和结尾、超出部分省略中间。
 * watch_stdin 为 true 时，stdin 被关闭就取消：杀掉整组进程并返回 -2。
 * 返回 0；无法启动返回 -1。 */
int plat_run_proc(char *const argv[], const char *cwd, char *const envp[], int timeout_s,
                  bool watch_stdin, size_t max_output, buf_t *out, proc_status *st);

/* Incremental Shell output. Callbacks are serialized (including on Windows);
 * data can be arbitrary bytes. The capture adapter remains bounded per call. */
typedef struct {
    void (*started)(void *ctx, int64_t pid);
    void (*output)(void *ctx, int stream, const void *data, size_t n);
    void *ctx;
 const char *input; /* optional bounded UTF-8 child stdin, <=4096 bytes */
} proc_stream;
int plat_run_proc_stream(char *const argv[], const char *cwd, char *const envp[], int timeout_s,
                          bool watch_stdin, size_t max_output, buf_t *out, proc_status *st,
                          const proc_stream *stream);

/* 运行一条命令，只收 stdout（内核用 --probe 检查执行器） */
int plat_probe(char *const argv[], int timeout_ms, buf_t *out);

/* Shell 用哪个程序、要不要加输出编码的前缀（Windows 的 PowerShell） */
char *plat_shell_exe(char *const envp[]);
const char *plat_shell_prefix(void);

/* ---- 管理命令（nano-kernel status / start / stop …）用的进程操作 ---- */

long plat_getpid(void);
/* 把标准错误（日志）改成追加写到文件（nano-kernel --log 文件） */
int  plat_log_to(const char *path);
/* 进程还在；name 非空时还要求程序名里含有 name（防止 pid 被别的程序复用） */
bool plat_pid_alive(long pid, const char *name);
/* 结束进程：POSIX 上 force 为假时发 SIGTERM，为真时发 SIGKILL；Windows 上直接结束 */
int  plat_pid_kill(long pid, bool force);
/* 运行一个命令（在 PATH 里找）并等它结束；quiet 时丢掉它的输出，否则用当前终端。
 * 返回退出码，启动不了返回 -1 */
int  plat_system(char *const argv[], bool quiet);
/* 在后台启动一个程序，脱离当前终端；标准错误追加到 log（NULL 表示丢掉）。返回 pid，失败 -1 */
long plat_spawn_bg(char *const argv[], const char *log);

/* ---- 网络、poll、唤醒（内核用；Windows 上只有 TCP） ---- */

typedef intptr_t na_sock;
#define NA_SOCK_NONE ((na_sock)-1)

/* 一项 poll；id 在 POSIX 上是 fd，在 Windows 上是 SOCKET（也可能是唤醒 socket）。 */
enum {
    NA_POLLIN = 0x01,
    NA_POLLOUT = 0x02,
    NA_POLLERR = 0x04,
    NA_POLLHUP = 0x08,
    NA_POLLNVAL = 0x10,
};

typedef struct {
    intptr_t id;
    short    events, revents;
} na_pollfd;

int  plat_poll(na_pollfd *fds, size_t n, int timeout_ms);
long plat_sock_read(na_sock s, void *buf, size_t n);      /* 0 = 对端关闭，-1 = 出错/EAGAIN */
long plat_sock_write(na_sock s, const void *buf, size_t n);
void plat_sock_close(na_sock s);
/* addr：unix:/path（只有 POSIX）、tcp:host:port 或 host:port。成功返回非阻塞 socket。 */
int  plat_connect(const char *addr, int timeout_ms, na_sock *out, char *err, size_t errlen);

/* 信号或控制台事件唤醒 poll 用的一对描述符 */
typedef struct {
    intptr_t rd, wr;
} na_wake;

int  plat_wake_open(na_wake *w);
void plat_wake_signal(na_wake *w);   /* 可以在信号处理函数里调用 */
void plat_wake_drain(na_wake *w);
void plat_wake_close(na_wake *w);
/* 安装 SIGINT/SIGTERM/SIGHUP（POSIX）或控制台 Ctrl-C 处理（Windows）；stop 置 1 并唤醒 poll */
int  plat_signals_install(na_wake *w, volatile int *stop);
void plat_signals_remove(void);

#endif
