/* sandbox.c —— Landlock + seccomp，见 sandbox.h
 *
 * Landlock 的结构体和常量照 Linux UAPI 自己定义一份，不依赖构建机上内核头文件的版本。
 */
#define _GNU_SOURCE   /* O_PATH */
#include "sandbox.h"

#include <errno.h>
#include <fcntl.h>
#include <linux/audit.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

/* ---- Landlock ---- */

#ifndef SYS_landlock_create_ruleset
#define SYS_landlock_create_ruleset 444
#define SYS_landlock_add_rule       445
#define SYS_landlock_restrict_self  446
#endif

#define LL_CREATE_RULESET_VERSION (1U << 0)
#define LL_RULE_PATH_BENEATH      1

#define LL_FS_EXECUTE     (1ULL << 0)
#define LL_FS_WRITE_FILE  (1ULL << 1)
#define LL_FS_READ_FILE   (1ULL << 2)
#define LL_FS_READ_DIR    (1ULL << 3)
#define LL_FS_ABI1_ALL    ((1ULL << 13) - 1)   /* 第 0~12 位：ABI 1 的全部权限 */
#define LL_FS_REFER       (1ULL << 13)         /* ABI 2：跨目录 rename / link */
#define LL_FS_TRUNCATE    (1ULL << 14)         /* ABI 3 */
#define LL_FS_IOCTL_DEV   (1ULL << 15)         /* ABI 5 */
#define LL_SCOPE_ABSTRACT_UNIX (1ULL << 0)     /* ABI 6 */
#define LL_SCOPE_SIGNAL        (1ULL << 1)     /* ABI 6 */

/* 只对普通文件有意义的权限；给文件（而不是目录）加规则时不能带其他权限 */
#define LL_FS_FILE_ONLY (LL_FS_EXECUTE | LL_FS_WRITE_FILE | LL_FS_READ_FILE | LL_FS_TRUNCATE | LL_FS_IOCTL_DEV)
#define LL_FS_READ      (LL_FS_EXECUTE | LL_FS_READ_FILE | LL_FS_READ_DIR)

struct ll_ruleset_attr {
    uint64_t handled_access_fs;
    uint64_t handled_access_net;
    uint64_t scoped;
};

struct ll_path_beneath_attr {
    uint64_t allowed_access;
    int32_t  parent_fd;
} __attribute__((packed));

int sb_landlock_abi(void)
{
    long abi = syscall(SYS_landlock_create_ruleset, NULL, 0, LL_CREATE_RULESET_VERSION);
    return abi < 0 ? 0 : (int)abi;
}

static int add_rule(int ruleset, const char *path, uint64_t access, uint64_t handled)
{
    int fd = open(path, O_PATH | O_CLOEXEC);
    if (fd < 0) return errno == ENOENT || errno == ENOTDIR ? 0 : -1;
    struct stat st;
    if (fstat(fd, &st) == 0 && !S_ISDIR(st.st_mode)) access &= LL_FS_FILE_ONLY;
    struct ll_path_beneath_attr pb = { .allowed_access = access & handled, .parent_fd = fd };
    int rc = (int)syscall(SYS_landlock_add_rule, ruleset, LL_RULE_PATH_BENEATH, &pb, 0);
    int saved = errno;
    /* 保留允许路径的 dentry，直到这次执行器退出。9p/WSL DrvFS 在最后一个
     * 引用关闭后可能重新创建 inode，Landlock 就认不出刚放行的目录了。
     * O_CLOEXEC 保证这些 O_PATH 描述符不会传给工具程序。 */
    if (rc < 0) close(fd);
    errno = saved;
    return rc;
}

static int apply_landlock(int abi, const sb_rules *r, char *err, size_t errlen)
{
    uint64_t fs = LL_FS_ABI1_ALL;
    if (abi >= 2) fs |= LL_FS_REFER;
    if (abi >= 3) fs |= LL_FS_TRUNCATE;
    if (abi >= 5) fs |= LL_FS_IOCTL_DEV;
    struct ll_ruleset_attr attr = {
        .handled_access_fs = fs,
        .scoped = abi >= 6 ? LL_SCOPE_ABSTRACT_UNIX | LL_SCOPE_SIGNAL : 0,
    };
    /* 老内核不认识的字段都是 0，按完整大小传也没问题 */
    int rs = (int)syscall(SYS_landlock_create_ruleset, &attr, sizeof attr, 0);
    if (rs < 0) {
        snprintf(err, errlen, "创建 Landlock 规则集失败：%s", strerror(errno));
        return -1;
    }
    for (size_t i = 0; i < r->nrw; i++) {
        if (add_rule(rs, r->rw[i], fs, fs) < 0) {
            snprintf(err, errlen, "Landlock 规则 %s：%s", r->rw[i], strerror(errno));
            close(rs);
            return -1;
        }
    }
    for (size_t i = 0; i < r->nro; i++) {
        if (add_rule(rs, r->ro[i], LL_FS_READ, fs) < 0) {
            snprintf(err, errlen, "Landlock 规则 %s：%s", r->ro[i], strerror(errno));
            close(rs);
            return -1;
        }
    }
    int rc = (int)syscall(SYS_landlock_restrict_self, rs, 0);
    int saved = errno;
    close(rs);
    if (rc < 0) {
        snprintf(err, errlen, "启用 Landlock 失败：%s", strerror(saved));
        return -1;
    }
    return 0;
}

/* ---- seccomp ---- */

#if defined(__x86_64__)
#define NATIVE_ARCH AUDIT_ARCH_X86_64
#elif defined(__aarch64__)
#define NATIVE_ARCH AUDIT_ARCH_AARCH64
#elif defined(__riscv) && __riscv_xlen == 64
#define NATIVE_ARCH AUDIT_ARCH_RISCV64
#endif

#ifdef NATIVE_ARCH
#ifndef __NR_io_uring_setup
#define __NR_io_uring_setup    425
#define __NR_io_uring_enter    426
#define __NR_io_uring_register 427
#endif

#define LOAD(field)  BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, field))
#define RET(v)       BPF_STMT(BPF_RET | BPF_K, (v))
#define DENY(e)      RET(SECCOMP_RET_ERRNO | (e))
/* 等于 nr 时执行下一条（拒绝），否则跳过它 */
#define IF_NR(nr)    BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, (nr), 0, 1)

static int apply_seccomp(char *err, size_t errlen)
{
    struct sock_filter f[] = {
        LOAD(arch),
        BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, NATIVE_ARCH, 1, 0),
        DENY(ENOSYS),                       /* 32 位兼容模式的系统调用一律拒绝 */
        LOAD(nr),
#ifdef __x86_64__
        BPF_JUMP(BPF_JMP | BPF_JGE | BPF_K, 0x40000000, 0, 1),
        DENY(ENOSYS),                       /* x32 */
#endif
        IF_NR(__NR_io_uring_setup),    DENY(ENOSYS),
        IF_NR(__NR_io_uring_enter),    DENY(ENOSYS),
        IF_NR(__NR_io_uring_register), DENY(ENOSYS),
        BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_socket, 0, 3),
        LOAD(args[0]),                      /* 小端：低 32 位就是 domain */
        BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, AF_UNIX, 0, 1),
        DENY(EPERM),
        RET(SECCOMP_RET_ALLOW),
    };
    struct sock_fprog prog = { .len = (unsigned short)(sizeof f / sizeof f[0]), .filter = f };
    if (prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &prog, 0, 0) < 0) {
        snprintf(err, errlen, "启用 seccomp 失败：%s", strerror(errno));
        return -1;
    }
    return 0;
}
#else
static int apply_seccomp(char *err, size_t errlen)
{
    snprintf(err, errlen, "这个 CPU 架构还不支持 seccomp 过滤");
    return -1;
}
#endif

int sb_apply(const sb_rules *r, char *err, size_t errlen)
{
    if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) < 0) {
        snprintf(err, errlen, "设置 no_new_privs 失败：%s", strerror(errno));
        return -1;
    }
    int abi = sb_landlock_abi();
    if (abi > 0 && apply_landlock(abi, r, err, errlen) < 0) return -1;
    if (apply_seccomp(err, errlen) < 0) return -1;
    return abi > 0 ? 0 : 1;
}
