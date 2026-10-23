/* plat_win.c —— 平台层的 Windows 实现，见 plat.h
 *
 * 全程用宽字符 API，只在边界上和 UTF-8 转换。路径的规范形式由这台机器决定：
 * 用 GetFinalPathNameByHandleW 解析符号链接和 junction，盘符大小写也由它给出。
 */
#define WIN32_LEAN_AND_MEAN
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0A00
#endif
#include <winsock2.h>
#include <ws2tcpip.h>
#include <windows.h>
#include <shellapi.h>
#include <io.h>
#include <fcntl.h>
#include <process.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <wchar.h>

#include "plat.h"

#ifndef EFBIG
#define EFBIG 27
#endif

#ifndef JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
#define JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 0x2000
#endif

static wchar_t *wdup(const wchar_t *s)
{
    if (!s) return NULL;
    size_t n = wcslen(s);
    wchar_t *p = xmalloc((n + 1) * sizeof *p);
    memcpy(p, s, (n + 1) * sizeof *p);
    return p;
}

static wchar_t *to_w(const char *s)
{
    if (!s) return NULL;
    int n = MultiByteToWideChar(CP_UTF8, 0, s, -1, NULL, 0);
    if (n <= 0) return NULL;
    wchar_t *w = xmalloc((size_t)n * sizeof *w);
    if (!MultiByteToWideChar(CP_UTF8, 0, s, -1, w, n)) { free(w); return NULL; }
    return w;
}

static char *to_u8(const wchar_t *s)
{
    if (!s) return NULL;
    int n = WideCharToMultiByte(CP_UTF8, 0, s, -1, NULL, 0, NULL, NULL);
    if (n <= 0) return NULL;
    char *u = xmalloc((size_t)n);
    if (!WideCharToMultiByte(CP_UTF8, 0, s, -1, u, n, NULL, NULL)) { free(u); return NULL; }
    return u;
}

static void set_err(char *err, size_t errlen, const char *what)
{
    if (!err) return;
    DWORD e = GetLastError();
    char *msg = NULL;
    FormatMessageA(FORMAT_MESSAGE_ALLOCATE_BUFFER | FORMAT_MESSAGE_FROM_SYSTEM | FORMAT_MESSAGE_IGNORE_INSERTS,
                   NULL, e, 0, (char *)&msg, 0, NULL);
    if (msg) {
        size_t n = strlen(msg);
        while (n && (msg[n - 1] == '\r' || msg[n - 1] == '\n')) msg[--n] = 0;
        snprintf(err, errlen, "%s：%s", what, msg);
        LocalFree(msg);
    } else {
        snprintf(err, errlen, "%s：错误 %lu", what, (unsigned long)e);
    }
}

/* 让 strerror(errno) 在 Windows 上也能说出个大概 */
static void set_errno_from_win(DWORD e)
{
    switch (e) {
    case ERROR_ACCESS_DENIED: errno = EACCES; break;
    case ERROR_FILE_NOT_FOUND:
    case ERROR_PATH_NOT_FOUND: errno = ENOENT; break;
    case ERROR_ALREADY_EXISTS:
    case ERROR_FILE_EXISTS: errno = EEXIST; break;
    case ERROR_INVALID_NAME:
    case ERROR_INVALID_PARAMETER: errno = EINVAL; break;
    default: errno = EIO; break;
    }
}

void plat_init(void)
{
    _setmode(_fileno(stdin), _O_BINARY);
    _setmode(_fileno(stdout), _O_BINARY);
    _setmode(_fileno(stderr), _O_BINARY);
    SetConsoleOutputCP(CP_UTF8);
    WSADATA wsa;
    WSAStartup(MAKEWORD(2, 2), &wsa);
}

void plat_shutdown(void)
{
    WSACleanup();
}

char **plat_args(int argc, char **argv, int *n)
{
    (void)argc;
    (void)argv;
    int wc = 0;
    wchar_t **w = CommandLineToArgvW(GetCommandLineW(), &wc);
    if (!w) { *n = 0; return NULL; }
    char **v = xcalloc((size_t)wc + 1, sizeof *v);
    for (int i = 0; i < wc; i++) v[i] = to_u8(w[i]);
    *n = wc;
    LocalFree(w);
    return v;
}

void plat_args_free(char **v, int n)
{
    for (int i = 0; i < n; i++) free(v[i]);
    free(v);
}

const char *plat_os(void) { return "windows"; }

const char *plat_arch(void)
{
#if defined(_M_ARM64) || defined(__aarch64__)
    return "arm64";
#else
    return "x86_64";
#endif
}

int64_t plat_now_ms(void)
{
    return (int64_t)GetTickCount64();
}

void plat_sleep_ms(int ms)
{
    Sleep((DWORD)ms);
}

/* ---- 宽字符路径辅助 ---- */

static bool is_abs_w(const wchar_t *p)
{
    if (!p || !*p) return false;
    if (p[0] == L'\\' || p[0] == L'/') return true;   /* 根相对或 UNC */
    if (((p[0] >= L'A' && p[0] <= L'Z') || (p[0] >= L'a' && p[0] <= L'z')) && p[1] == L':' &&
        (p[2] == L'\\' || p[2] == L'/'))
        return true;
    return false;
}

static wchar_t *curdir_w(void)
{
    DWORD n = GetCurrentDirectoryW(0, NULL);
    if (!n) return wdup(L".");
    wchar_t *b = xmalloc((size_t)n * sizeof *b);
    if (!GetCurrentDirectoryW(n, b)) { free(b); return wdup(L"."); }
    return b;
}

/* 解析已存在路径的最终形式，去掉 \\?\ 前缀；失败返回 NULL */
static wchar_t *final_path_w(const wchar_t *path)
{
    HANDLE h = CreateFileW(path, 0, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, NULL,
                           OPEN_EXISTING, FILE_FLAG_BACKUP_SEMANTICS, NULL);
    if (h == INVALID_HANDLE_VALUE) return NULL;
    DWORD n = GetFinalPathNameByHandleW(h, NULL, 0, FILE_NAME_NORMALIZED | VOLUME_NAME_DOS);
    if (!n) { CloseHandle(h); return NULL; }
    wchar_t *buf = xmalloc(((size_t)n + 2) * sizeof *buf);
    DWORD r = GetFinalPathNameByHandleW(h, buf, n + 1, FILE_NAME_NORMALIZED | VOLUME_NAME_DOS);
    CloseHandle(h);
    if (!r || r > n) { free(buf); return NULL; }
    buf[r] = 0;
    wchar_t *res;
    if (!wcsncmp(buf, L"\\\\?\\UNC\\", 8)) {
        size_t l = wcslen(buf + 8);
        res = xmalloc((l + 3) * sizeof *res);
        res[0] = L'\\'; res[1] = L'\\';
        memcpy(res + 2, buf + 8, (l + 1) * sizeof *res);
    } else if (!wcsncmp(buf, L"\\\\?\\", 4)) {
        res = wdup(buf + 4);
    } else {
        res = wdup(buf);
    }
    free(buf);
    return res;
}

static wchar_t *full_path_w(const wchar_t *p)
{
    DWORD n = GetFullPathNameW(p, 0, NULL, NULL);
    if (!n) return NULL;
    wchar_t *out = xmalloc(((size_t)n + 2) * sizeof *out);
    DWORD r = GetFullPathNameW(p, n + 1, out, NULL);
    if (!r || r > n) { free(out); return NULL; }
    size_t l = wcslen(out);
    while (l > 1 && (out[l - 1] == L'\\' || out[l - 1] == L'/') && !(l == 3 && out[1] == L':')) l--;
    out[l] = 0;
    return out;
}

char *plat_self_exe(void)
{
    DWORD cap = 1024;
    wchar_t *buf = NULL;
    for (;;) {
        buf = xrealloc(buf, (size_t)cap * sizeof *buf);
        DWORD n = GetModuleFileNameW(NULL, buf, cap);
        if (n && n < cap) { buf[n] = 0; break; }
        if (n < cap) { free(buf); return NULL; }
        cap *= 2;
    }
    wchar_t *real = final_path_w(buf);
    if (!real) real = full_path_w(buf);
    free(buf);
    char *u = to_u8(real);
    free(real);
    return u;
}

int plat_exec(const char *path, char **argv)
{
    (void)path;
    (void)argv;
    errno = ENOSYS;   /* Windows 上没有 exec：退出码 75 让 run-kernel.ps1 重新拉起 */
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

char *plat_home(void)
{
    DWORD n = GetEnvironmentVariableW(L"USERPROFILE", NULL, 0);
    if (n) {
        wchar_t *b = xmalloc((size_t)n * sizeof *b);
        if (GetEnvironmentVariableW(L"USERPROFILE", b, n)) {
            size_t l = wcslen(b);
            while (l && (b[l - 1] == L' ' || b[l - 1] == L'\t' || b[l - 1] == L'\r' || b[l - 1] == L'\n')) b[--l] = 0;
            char *u = to_u8(b);
            free(b);
            if (u && *u) return u;
            free(u);
        } else {
            free(b);
        }
    }
    wchar_t drive[8] = L"", path[1024] = L"";
    GetEnvironmentVariableW(L"HOMEDRIVE", drive, 8);
    GetEnvironmentVariableW(L"HOMEPATH", path, 1024);
    if (*drive || *path) {
        wchar_t *b = xmalloc((wcslen(drive) + wcslen(path) + 1) * sizeof *b);
        wcscpy(b, drive);
        wcscat(b, path);
        char *u = to_u8(b);
        free(b);
        if (u && *u) {
            size_t l = strlen(u);
            while (l && (u[l - 1] == ' ' || u[l - 1] == '\t')) u[--l] = 0;
            return u;
        }
        free(u);
    }
    return xstrdup("C:\\");
}

char *plat_hostname(void)
{
    wchar_t buf[256] = L"kernel";
    DWORD n = 256;
    if (!GetComputerNameW(buf, &n)) wcscpy(buf, L"kernel");
    return to_u8(buf);
}

bool plat_is_abs(const char *p)
{
    wchar_t *w = to_w(p);
    bool b = is_abs_w(w);
    free(w);
    return b;
}

bool plat_is_root(const char *p)
{
    if (!p) return false;
    size_t n = strlen(p);
    if (n == 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')) return true;
    return n == 1 && (p[0] == '\\' || p[0] == '/');
}

char *plat_expand_home(const char *p)
{
    if (p && p[0] == '~' && (p[1] == 0 || p[1] == '\\' || p[1] == '/')) {
        char *home = plat_home();
        char *out = p[1] ? plat_join(home, p + 2) : xstrdup(home);
        free(home);
        return out;
    }
    return xstrdup(p ? p : "");
}

char *plat_dirname(const char *p)
{
    if (!p || !*p) return xstrdup(".");
    wchar_t *w = to_w(p);
    wchar_t *slash = wcsrchr(w, L'\\');
    wchar_t *fslash = wcsrchr(w, L'/');
    if (fslash > slash) slash = fslash;
    if (!slash) { free(w); return xstrdup("."); }
    if (slash == w) { free(w); return xstrdup("\\"); }
    if (slash == w + 2 && w[1] == L':') {
        slash[1] = 0;   /* C:\ */
        char *u = to_u8(w);
        free(w);
        return u;
    }
    *slash = 0;
    char *u = to_u8(w);
    free(w);
    return u;
}

char *plat_join(const char *dir, const char *name)
{
    if (!dir || !*dir) return xstrdup(name ? name : "");
    if (!name || !*name) return xstrdup(dir);
    if (plat_is_abs(name)) return xstrdup(name);
    size_t l = strlen(dir);
    if (dir[l - 1] == '\\' || dir[l - 1] == '/') return xasprintf("%s%s", dir, name);
    return xasprintf("%s\\%s", dir, name);
}

char *plat_abspath(const char *base, const char *p)
{
    wchar_t *pw = to_w(p && *p ? p : ".");
    if (!pw) return NULL;
    wchar_t *full;
    if (is_abs_w(pw)) {
        full = full_path_w(pw);
    } else {
        wchar_t *b = base && *base ? to_w(base) : curdir_w();
        if (!b) { free(pw); return NULL; }
        wchar_t *joined = xmalloc((wcslen(b) + wcslen(pw) + 2) * sizeof *joined);
        wcscpy(joined, b);
        size_t l = wcslen(joined);
        if (l && joined[l - 1] != L'\\' && joined[l - 1] != L'/') wcscat(joined, L"\\");
        wcscat(joined, pw);
        full = full_path_w(joined);
        free(joined);
        free(b);
    }
    free(pw);
    if (!full) return NULL;
    char *u = to_u8(full);
    free(full);
    return u;
}

char *plat_canonical(const char *abs)
{
    wchar_t *full = to_w(abs);
    if (!full) return NULL;
    wchar_t *p = full_path_w(full);
    free(full);
    if (!p) return NULL;
    size_t cut = wcslen(p);
    for (;;) {
        wchar_t saved = p[cut];
        p[cut] = 0;
        wchar_t *real = final_path_w(p);
        p[cut] = saved;
        if (real) {
            wchar_t *res;
            if (!saved) {
                res = wdup(real);
            } else {
                size_t rl = wcslen(real);
                bool rsep = rl && (real[rl - 1] == L'\\' || real[rl - 1] == L'/');
                const wchar_t *rest = p + cut;
                if (rsep && (*rest == L'\\' || *rest == L'/')) rest++;
                res = xmalloc((rl + wcslen(rest) + 2) * sizeof *res);
                wcscpy(res, real);
                if (!rsep && *rest != L'\\' && *rest != L'/') wcscat(res, L"\\");
                wcscat(res, rest);
            }
            free(real);
            free(p);
            char *u = to_u8(res);
            free(res);
            return u;
        }
        /* 去掉最后一段，继续找存在的上级 */
        bool drive_root = cut == 3 && p[1] == L':';
        if (drive_root) break;
        while (cut > 0 && p[cut - 1] != L'\\' && p[cut - 1] != L'/') cut--;
        if (cut > 3 || (cut > 0 && !(cut == 3 && p[1] == L':'))) cut--;
        if (cut == 0) break;
    }
    free(p);
    return NULL;
}

bool plat_inside(const char *path, const char *dir)
{
    if (!path || !dir) return false;
    wchar_t *a = to_w(path), *b = to_w(dir);
    if (!a || !b) { free(a); free(b); return false; }
    size_t n = wcslen(b);
    if (n == 0) { free(a); free(b); return false; }
    bool ok = _wcsnicmp(a, b, n) == 0;
    if (ok) {
        bool drive_root = n == 3 && b[1] == L':' && (b[2] == L'\\' || b[2] == L'/');
        ok = drive_root || a[n] == 0 || a[n] == L'\\' || a[n] == L'/';
    }
    free(a);
    free(b);
    return ok;
}

bool plat_path_eq(const char *a, const char *b)
{
    wchar_t *x = to_w(a), *y = to_w(b);
    bool eq = x && y && _wcsicmp(x, y) == 0;
    free(x);
    free(y);
    return eq;
}

const char *plat_sep(void) { return "\\"; }

char *plat_relpath(const char *path, const char *base)
{
    if (!plat_inside(path, base)) return NULL;
    wchar_t *a = to_w(path), *b = to_w(base);
    size_t n = wcslen(b);
    wchar_t *r;
    if (!a[n] || wcscmp(a, b) == 0) r = wdup(L".");
    else r = wdup(a + n + 1);
    char *u = to_u8(r);
    free(a); free(b); free(r);
    return u;
}

/* ---- 文件 ---- */

bool plat_stat(const char *p, plat_stat_t *st)
{
    memset(st, 0, sizeof *st);
    wchar_t *w = to_w(p);
    if (!w) return false;
    WIN32_FILE_ATTRIBUTE_DATA d;
    bool ok = GetFileAttributesExW(w, GetFileExInfoStandard, &d) != 0;
    free(w);
    if (!ok) return false;
    st->exists = true;
    st->is_dir = (d.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0;
    st->size = ((int64_t)d.nFileSizeHigh << 32) | d.nFileSizeLow;
    return true;
}

bool plat_is_executable(const char *p)
{
    plat_stat_t st;
    return plat_stat(p, &st) && !st.is_dir;
}

static size_t mkdir_start(const wchar_t *p)
{
    if (p[0] && p[1] == L':') return 3;
    if ((p[0] == L'\\' || p[0] == L'/') && (p[1] == L'\\' || p[1] == L'/')) {
        size_t k = 2;
        while (p[k] && p[k] != L'\\' && p[k] != L'/') k++;
        if (p[k]) k++;
        while (p[k] && p[k] != L'\\' && p[k] != L'/') k++;
        return k;
    }
    return 0;
}

static int makedirs_w(const wchar_t *path)
{
    wchar_t *p = wdup(path);
    size_t start = mkdir_start(p);
    size_t n = wcslen(p);
    for (size_t i = start; i < n; i++) {
        if (p[i] != L'\\' && p[i] != L'/') continue;
        wchar_t c = p[i];
        p[i] = 0;
        if (i > start && !CreateDirectoryW(p, NULL) && GetLastError() != ERROR_ALREADY_EXISTS) {
            int e = (int)GetLastError();
            free(p);
            return e ? -1 : 0;
        }
        p[i] = c;
    }
    if (!CreateDirectoryW(p, NULL) && GetLastError() != ERROR_ALREADY_EXISTS) {
        free(p);
        return -1;
    }
    free(p);
    return 0;
}

int plat_makedirs(const char *p)
{
    wchar_t *w = to_w(p);
    if (!w) { SetLastError(ERROR_INVALID_NAME); errno = EINVAL; return -1; }
    DWORD before = GetLastError();
    int rc = makedirs_w(w);
    DWORD e = GetLastError();
    free(w);
    if (rc < 0) set_errno_from_win(e ? e : before);
    return rc;
}

int plat_mkdir(const char *p)
{
    wchar_t *w = to_w(p);
    if (!w) { errno = EINVAL; return -1; }
    int ok = CreateDirectoryW(w, NULL) != 0;
    if (!ok) set_errno_from_win(GetLastError());
    free(w);
    return ok ? 0 : -1;
}

static int remove_tree_w(const wchar_t *path)
{
    DWORD attr = GetFileAttributesW(path);
    if (attr == INVALID_FILE_ATTRIBUTES) {
        return GetLastError() == ERROR_FILE_NOT_FOUND || GetLastError() == ERROR_PATH_NOT_FOUND ? 0 : -1;
    }
    if (!(attr & FILE_ATTRIBUTE_DIRECTORY)) {
        SetFileAttributesW(path, FILE_ATTRIBUTE_NORMAL);
        return DeleteFileW(path) ? 0 : -1;
    }
    if (attr & FILE_ATTRIBUTE_REPARSE_POINT) return RemoveDirectoryW(path) ? 0 : -1;
    size_t l = wcslen(path);
    wchar_t *pat = xmalloc((l + 3) * sizeof *pat);
    wcscpy(pat, path);
    if (l && pat[l - 1] != L'\\' && pat[l - 1] != L'/') wcscat(pat, L"\\");
    wcscat(pat, L"*");
    WIN32_FIND_DATAW fd;
    HANDLE h = FindFirstFileW(pat, &fd);
    free(pat);
    if (h != INVALID_HANDLE_VALUE) {
        do {
            if (!wcscmp(fd.cFileName, L".") || !wcscmp(fd.cFileName, L"..")) continue;
            wchar_t *sub = xmalloc((l + wcslen(fd.cFileName) + 3) * sizeof *sub);
            wcscpy(sub, path);
            if (l && sub[l - 1] != L'\\' && sub[l - 1] != L'/') wcscat(sub, L"\\");
            wcscat(sub, fd.cFileName);
            remove_tree_w(sub);
            free(sub);
        } while (FindNextFileW(h, &fd));
        FindClose(h);
    }
    return RemoveDirectoryW(path) ? 0 : -1;
}

int plat_remove_tree(const char *p)
{
    if (!p || !*p) return 0;
    wchar_t *w = to_w(p);
    if (!w) return -1;
    int rc = remove_tree_w(w);
    free(w);
    return rc;
}

int plat_rename(const char *from, const char *to)
{
    wchar_t *a = to_w(from), *b = to_w(to);
    if (!a || !b) { free(a); free(b); return -1; }
    int ok = MoveFileExW(a, b, MOVEFILE_REPLACE_EXISTING) != 0;
    free(a); free(b);
    return ok ? 0 : -1;
}

int plat_unlink(const char *p)
{
    wchar_t *w = to_w(p);
    if (!w) return -1;
    int ok = DeleteFileW(w) != 0;
    free(w);
    return ok ? 0 : -1;
}

int plat_write_atomic(const char *path, const char *data, size_t n, int mode)
{
    (void)mode;
    char *dir = plat_dirname(path);
    if (!dir) return -1;
    wchar_t *wdir = to_w(dir);
    wchar_t *wpath = to_w(path);
    free(dir);
    if (!wdir || !wpath) { free(wdir); free(wpath); return -1; }

    wchar_t *tmp = xmalloc((wcslen(wdir) + 64) * sizeof *tmp);
    HANDLE h = INVALID_HANDLE_VALUE;
    for (unsigned i = 0; i < 1000; i++) {
        _snwprintf(tmp, wcslen(wdir) + 64, L"%ls\\.nano-%lu-%u.tmp",
                   wdir, (unsigned long)GetCurrentProcessId(), i);
        h = CreateFileW(tmp, GENERIC_WRITE, 0, NULL, CREATE_NEW, FILE_ATTRIBUTE_NORMAL, NULL);
        if (h != INVALID_HANDLE_VALUE) break;
        if (GetLastError() != ERROR_FILE_EXISTS && GetLastError() != ERROR_ALREADY_EXISTS) break;
    }
    free(wdir);
    if (h == INVALID_HANDLE_VALUE) { free(tmp); free(wpath); return -1; }
    size_t off = 0;
    int rc = 0;
    while (off < n) {
        DWORD chunk = (DWORD)((n - off) > (1u << 20) ? (1u << 20) : (n - off));
        DWORD wrote = 0;
        if (!WriteFile(h, data + off, chunk, &wrote, NULL) || wrote == 0) { rc = -1; break; }
        off += wrote;
    }
    if (rc == 0 && !FlushFileBuffers(h)) rc = -1;
    CloseHandle(h);
    if (rc == 0 && !MoveFileExW(tmp, wpath, MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) rc = -1;
    if (rc) DeleteFileW(tmp);
    free(tmp);
    free(wpath);
    return rc;
}

int plat_read_file(const char *path, buf_t *out, size_t max)
{
    wchar_t *w = to_w(path);
    if (!w) return -1;
    HANDLE h = CreateFileW(w, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                           NULL, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, NULL);
    free(w);
    if (h == INVALID_HANDLE_VALUE) return -1;
    char tmp[65536];
    for (;;) {
        DWORD got = 0;
        if (!ReadFile(h, tmp, sizeof tmp, &got, NULL)) { CloseHandle(h); return -1; }
        if (!got) break;
        if (out->len + got > max) { CloseHandle(h); errno = EFBIG; return -1; }
        buf_append(out, tmp, got);
    }
    CloseHandle(h);
    return 0;
}

bool plat_read_dir(const char *path, size_t max, plat_dirent **ents, size_t *n, bool *truncated,
                   char *err, size_t errlen)
{
    *ents = NULL;
    *n = 0;
    *truncated = false;
    wchar_t *w = to_w(path);
    if (!w) { if (err) snprintf(err, errlen, "路径不是合法的 UTF-8"); return false; }
    size_t l = wcslen(w);
    wchar_t *pat = xmalloc((l + 3) * sizeof *pat);
    wcscpy(pat, w);
    if (l && pat[l - 1] != L'\\' && pat[l - 1] != L'/') wcscat(pat, L"\\");
    wcscat(pat, L"*");
    WIN32_FIND_DATAW fd;
    HANDLE h = FindFirstFileW(pat, &fd);
    free(pat);
    free(w);
    if (h == INVALID_HANDLE_VALUE) {
        if (err) set_err(err, errlen, "打开目录失败");
        return false;
    }
    do {
        if (!wcscmp(fd.cFileName, L".") || !wcscmp(fd.cFileName, L"..")) continue;
        if (*n >= max) { *truncated = true; continue; }
        *ents = xrealloc(*ents, (*n + 1) * sizeof **ents);
        (*ents)[*n].name = to_u8(fd.cFileName);
        (*ents)[*n].dir = (fd.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0;
        if (!(*ents)[*n].name) (*ents)[*n].name = xstrdup("?");
        (*n)++;
    } while (FindNextFileW(h, &fd));
    FindClose(h);
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
    DWORD need = GetLogicalDriveStringsW(0, NULL);
    if (!need) return NULL;
    wchar_t *buf = xmalloc(((size_t)need + 1) * sizeof *buf);
    if (!GetLogicalDriveStringsW(need, buf)) { free(buf); return NULL; }
    char **v = NULL;
    size_t cnt = 0;
    for (wchar_t *p = buf; *p; p += wcslen(p) + 1) {
        UINT t = GetDriveTypeW(p);
        if (t == DRIVE_UNKNOWN || t == DRIVE_NO_ROOT_DIR) continue;
        v = xrealloc(v, (cnt + 1) * sizeof *v);
        v[cnt++] = to_u8(p);
    }
    free(buf);
    *n = cnt;
    return v;
}

void plat_drives_free(char **v, size_t n)
{
    for (size_t i = 0; i < n; i++) free(v[i]);
    free(v);
}

struct plat_file {
    HANDLE h;
};

plat_file *plat_open_read(const char *path)
{
    wchar_t *w = to_w(path);
    if (!w) return NULL;
    HANDLE h = CreateFileW(w, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                           NULL, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, NULL);
    free(w);
    if (h == INVALID_HANDLE_VALUE) return NULL;
    plat_file *f = xmalloc(sizeof *f);
    f->h = h;
    return f;
}

long plat_file_read(plat_file *f, void *buf, size_t n)
{
    DWORD got = 0;
    if (!ReadFile(f->h, buf, (DWORD)n, &got, NULL)) return -1;
    return (long)got;
}

void plat_file_close(plat_file *f)
{
    if (!f) return;
    CloseHandle(f->h);
    free(f);
}

/* ---- 查找、环境 ---- */

char *plat_find_in_path(char *const envp[], const char *name)
{
    if (!name || !*name) return NULL;
    wchar_t *wn = to_w(name);
    if (!wn) return NULL;
    if (wcschr(wn, L'\\') || wcschr(wn, L'/') || wcschr(wn, L':')) {
        DWORD a = GetFileAttributesW(wn);
        char *u = (a != INVALID_FILE_ATTRIBUTES && !(a & FILE_ATTRIBUTE_DIRECTORY)) ? to_u8(wn) : NULL;
        free(wn);
        return u;
    }
    const char *path = NULL;
    for (char *const *e = envp; e && *e; e++)
        if (!_strnicmp(*e, "PATH=", 5)) path = *e + 5;
    if (!path || !*path) path = "C:\\Windows\\System32;C:\\Windows";

    const char *pathext = ".COM;.EXE;.BAT;.CMD";
    for (char *const *e = envp; e && *e; e++)
        if (!_strnicmp(*e, "PATHEXT=", 8)) pathext = *e + 8;

    char *dup = xstrdup(path);
    char *hit = NULL;
    for (char *d = dup; d && *d && !hit;) {
        char *sep = strchr(d, ';');
        if (sep) *sep = 0;
        if (*d) {
            for (int with_ext = -1; with_ext < 16 && !hit; with_ext++) {
                char *cand;
                if (with_ext < 0) {
                    cand = xasprintf("%s\\%s", d, name);
                } else {
                    /* 第 with_ext 个扩展名；-1 表示不带扩展名先试一次 */
                    const char *x = pathext;
                    int k = 0;
                    while (x && *x && k < with_ext) {
                        const char *semi = strchr(x, ';');
                        x = semi ? semi + 1 : NULL;
                        k++;
                    }
                    if (!x || !*x) break;
                    const char *semi = strchr(x, ';');
                    size_t xl = semi ? (size_t)(semi - x) : strlen(x);
                    cand = xasprintf("%s\\%s%.*s", d, name, (int)xl, x);
                }
                wchar_t *wt = to_w(cand);
                DWORD a = wt ? GetFileAttributesW(wt) : INVALID_FILE_ATTRIBUTES;
                free(wt);
                if (a != INVALID_FILE_ATTRIBUTES && !(a & FILE_ATTRIBUTE_DIRECTORY)) hit = cand;
                else free(cand);
            }
        }
        d = sep ? sep + 1 : NULL;
    }
    free(dup);
    free(wn);
    return hit;
}

static bool env_key_is(const wchar_t *e, const wchar_t *key)
{
    size_t n = wcslen(key);
    return !_wcsnicmp(e, key, n) && e[n] == L'=';
}

char **plat_build_env(const char *home, const char *tmp, const char *cache)
{
    (void)cache;
    char **env = NULL;
    size_t n = 0;
    wchar_t *block = GetEnvironmentStringsW();
    bool has_path = false;
    if (block) {
        for (wchar_t *p = block; *p; p += wcslen(p) + 1) {
            static const wchar_t *keep[] = {
                L"SystemRoot", L"SystemDrive", L"ComSpec", L"PATHEXT", L"PATH", L"USERPROFILE",
                L"LOCALAPPDATA", L"APPDATA", L"USERNAME", L"USERDOMAIN",
                L"HOMEDRIVE", L"HOMEPATH", L"NUMBER_OF_PROCESSORS", L"PROCESSOR_ARCHITECTURE",
                L"OS", L"LANG", L"LC_ALL", L"LC_CTYPE", L"TZ", L"NANO_HOME", L"ProgramFiles",
                L"ProgramFiles(x86)", L"ProgramW6432", L"ProgramData", L"OneDrive",
            };
            bool k = false;
            for (size_t i = 0; i < sizeof keep / sizeof *keep; i++)
                if (env_key_is(p, keep[i])) { k = true; break; }
            if (!k) continue;
            if (env_key_is(p, L"PATH")) has_path = true;
            char *u = to_u8(p);
            if (!u) continue;
            env = xrealloc(env, (n + 2) * sizeof *env);
            env[n++] = u;
            env[n] = NULL;
        }
        FreeEnvironmentStringsW(block);
    }
    if (!has_path) {
        env = xrealloc(env, (n + 2) * sizeof *env);
        env[n++] = xstrdup("PATH=C:\\Windows\\System32;C:\\Windows");
        env[n] = NULL;
    }
    env = xrealloc(env, (n + 8) * sizeof *env);
    env[n++] = xasprintf("HOME=%s", home);
    env[n++] = xasprintf("TEMP=%s", tmp);
    env[n++] = xasprintf("TMP=%s", tmp);
    env[n++] = xasprintf("TMPDIR=%s", tmp);
    env[n++] = xstrdup("NO_COLOR=1");
    env[n++] = xstrdup("GIT_TERMINAL_PROMPT=0");
    env[n] = NULL;
    return env;
}

/* ---- 子进程 ---- */

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

/* 按 Windows 的规则给一个参数加引号 */
static size_t arg_quoted_len(const wchar_t *a)
{
    bool q = !*a || wcspbrk(a, L" \t\n\v\"") != NULL;
    if (!q) return wcslen(a);
    size_t n = 2, bs = 0;
    for (const wchar_t *p = a; *p; p++) {
        if (*p == L'\\') { bs++; continue; }
        if (*p == L'"') { n += 2 * bs + 1; bs = 0; n += 1; continue; }
        n += bs + 1;
        bs = 0;
    }
    return n + 2 * bs;
}

static wchar_t *arg_quoted_copy(wchar_t *out, const wchar_t *a)
{
    bool q = !*a || wcspbrk(a, L" \t\n\v\"") != NULL;
    if (!q) {
        size_t n = wcslen(a);
        memcpy(out, a, n * sizeof *out);
        return out + n;
    }
    *out++ = L'"';
    size_t bs = 0;
    for (const wchar_t *p = a; *p; p++) {
        if (*p == L'\\') { bs++; continue; }
        if (*p == L'"') {
            for (size_t i = 0; i < 2 * bs + 1; i++) *out++ = L'\\';
            *out++ = L'"';
            bs = 0;
            continue;
        }
        for (size_t i = 0; i < bs; i++) *out++ = L'\\';
        bs = 0;
        *out++ = *p;
    }
    for (size_t i = 0; i < 2 * bs; i++) *out++ = L'\\';
    *out++ = L'"';
    return out;
}

static wchar_t *make_cmdline(char *const argv[])
{
    size_t n = 0;
    for (int i = 0; argv[i]; i++) {
        wchar_t *a = to_w(argv[i]);
        n += arg_quoted_len(a) + 1;
        free(a);
    }
    wchar_t *line = xmalloc((n + 2) * sizeof *line);
    wchar_t *w = line;
    for (int i = 0; argv[i]; i++) {
        wchar_t *a = to_w(argv[i]);
        w = arg_quoted_copy(w, a);
        free(a);
        if (argv[i + 1]) *w++ = L' ';
    }
    *w = 0;
    return line;
}

static wchar_t *make_envblock(char *const envp[])
{
    if (!envp) return NULL;
    size_t n = 0;
    for (int i = 0; envp[i]; i++) {
        wchar_t *e = to_w(envp[i]);
        n += wcslen(e) + 1;
        free(e);
    }
    wchar_t *block = xmalloc((n + 2) * sizeof *block);
    wchar_t *w = block;
    for (int i = 0; envp[i]; i++) {
        wchar_t *e = to_w(envp[i]);
        size_t l = wcslen(e);
        memcpy(w, e, l * sizeof *w);
        w += l;
        *w++ = 0;
        free(e);
    }
    *w = 0;
    return block;
}

typedef struct {
    HANDLE proc, thread, job, out_read, out_write, nul_in, nul_out;
} child_t;

static int spawn_child(char *const argv[], const char *cwd, char *const envp[], bool combine,
                       child_t *c, char *err, size_t errlen)
{
    memset(c, 0, sizeof *c);
    c->proc = c->thread = c->job = INVALID_HANDLE_VALUE;
    c->out_read = c->out_write = c->nul_in = c->nul_out = INVALID_HANDLE_VALUE;
    wchar_t *line = make_cmdline(argv);
    wchar_t *cw = cwd && *cwd ? to_w(cwd) : NULL;
    wchar_t *env = make_envblock(envp);
    SECURITY_ATTRIBUTES sa = { sizeof sa, NULL, TRUE };
    HANDLE out_r = INVALID_HANDLE_VALUE, out_w = INVALID_HANDLE_VALUE;
    if (!CreatePipe(&out_r, &out_w, &sa, 0)) {
        set_err(err, errlen, "创建管道失败");
        goto fail;
    }
    SetHandleInformation(out_r, HANDLE_FLAG_INHERIT, 0);
    HANDLE nul_in = CreateFileW(L"NUL", GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa,
                                OPEN_EXISTING, 0, NULL);
    HANDLE nul_out = CreateFileW(L"NUL", GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa,
                                 OPEN_EXISTING, 0, NULL);

    STARTUPINFOEXW si;
    ZeroMemory(&si, sizeof si);
    si.StartupInfo.cb = sizeof si;
    si.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    si.StartupInfo.hStdInput = nul_in;
    si.StartupInfo.hStdOutput = out_w;
    si.StartupInfo.hStdError = combine ? out_w : nul_out;
    SIZE_T attr_size = 0;
    InitializeProcThreadAttributeList(NULL, 1, 0, &attr_size);
    si.lpAttributeList = xmalloc(attr_size ? attr_size : 1);
    bool have_attr = attr_size && InitializeProcThreadAttributeList(si.lpAttributeList, 1, 0, &attr_size);
    if (have_attr) {
        HANDLE keep[3] = { out_w, nul_in, nul_out };
        SIZE_T keepn = combine ? 2 : 3;
        UpdateProcThreadAttribute(si.lpAttributeList, 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
                                  keep, keepn * sizeof(HANDLE), NULL, NULL);
    } else {
        si.StartupInfo.cb = sizeof(STARTUPINFO);
    }
    DWORD flags = CREATE_SUSPENDED | CREATE_UNICODE_ENVIRONMENT | CREATE_NO_WINDOW;
    if (have_attr) flags |= EXTENDED_STARTUPINFO_PRESENT;
    PROCESS_INFORMATION pi;
    ZeroMemory(&pi, sizeof pi);
    BOOL ok = CreateProcessW(NULL, line, NULL, NULL, TRUE, flags, env, cw, &si.StartupInfo, &pi);
    DWORD e = GetLastError();
    if (have_attr) DeleteProcThreadAttributeList(si.lpAttributeList);
    free(si.lpAttributeList);
    if (!ok) {
        SetLastError(e);
        set_err(err, errlen, "启动命令失败");
        if (nul_in != INVALID_HANDLE_VALUE) CloseHandle(nul_in);
        if (nul_out != INVALID_HANDLE_VALUE) CloseHandle(nul_out);
        CloseHandle(out_r);
        CloseHandle(out_w);
        goto fail;
    }
    CloseHandle(nul_in);
    CloseHandle(nul_out);
    CloseHandle(out_w);

    HANDLE job = CreateJobObjectW(NULL, NULL);
    if (job) {
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION li;
        ZeroMemory(&li, sizeof li);
        li.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        SetInformationJobObject(job, JobObjectExtendedLimitInformation, &li, sizeof li);
        if (!AssignProcessToJobObject(job, pi.hProcess)) {
            CloseHandle(job);
            job = NULL;
        }
    }
    ResumeThread(pi.hThread);
    CloseHandle(pi.hThread);

    c->proc = pi.hProcess;
    c->job = job;
    c->out_read = out_r;
    free(line);
    free(cw);
    free(env);
    return 0;

fail:
    free(line);
    free(cw);
    free(env);
    return -1;
}

typedef struct {
    HANDLE h;
    capture *cap;
    HANDLE done;
} reader_arg;

static DWORD WINAPI reader_thread(LPVOID p)
{
    reader_arg *r = p;
    char buf[16384];
    for (;;) {
        DWORD got = 0;
        if (!ReadFile(r->h, buf, sizeof buf, &got, NULL) || !got) break;
        cap_add(r->cap, buf, got);
    }
    SetEvent(r->done);
    return 0;
}

typedef struct {
    volatile LONG *flag;
    HANDLE ev;
} watch_arg;

static DWORD WINAPI watch_thread(LPVOID p)
{
    watch_arg *w = p;
    char buf[256];
    DWORD got = 0;
    ReadFile(GetStdHandle(STD_INPUT_HANDLE), buf, sizeof buf, &got, NULL);
    InterlockedExchange(w->flag, 1);
    SetEvent(w->ev);
    return 0;
}

static void kill_child(child_t *c)
{
    if (c->job != INVALID_HANDLE_VALUE && c->job) TerminateJobObject(c->job, 0);
    else if (c->proc != INVALID_HANDLE_VALUE && c->proc) TerminateProcess(c->proc, 1);
}

int plat_run_proc(char *const argv[], const char *cwd, char *const envp[], int timeout_s,
                  bool watch_stdin, size_t max_output, buf_t *out, proc_status *st)
{
    memset(st, 0, sizeof *st);
    child_t c;
    char err[512] = "";
    if (spawn_child(argv, cwd, envp, true, &c, err, sizeof err) < 0) {
        buf_puts(out, err);
        return -1;
    }
    capture cap = { .head_max = max_output * 3 / 4, .tail_max = max_output / 4 };
    HANDLE done = CreateEventW(NULL, TRUE, FALSE, NULL);
    reader_arg ra = { .h = c.out_read, .cap = &cap, .done = done };
    HANDLE rt = CreateThread(NULL, 0, reader_thread, &ra, 0, NULL);
    HANDLE ev = CreateEventW(NULL, TRUE, FALSE, NULL);
    volatile LONG cancelled = 0;
    HANDLE wt = NULL;
    watch_arg wa = { .flag = &cancelled, .ev = ev };
    if (watch_stdin) wt = CreateThread(NULL, 0, watch_thread, &wa, 0, NULL);

    int64_t deadline = plat_now_ms() + (int64_t)timeout_s * 1000;
    for (;;) {
        int64_t left = deadline - plat_now_ms();
        DWORD wait = left <= 0 ? 0 : (left > 1000 ? 1000 : (DWORD)left);
        DWORD r = WaitForSingleObject(c.proc, wait);
        if (r == WAIT_OBJECT_0) {
            DWORD code = 1;
            GetExitCodeProcess(c.proc, &code);
            st->exit_code = (int)code;
            break;
        }
        if (InterlockedCompareExchange(&cancelled, 1, 1) == 1) {
            kill_child(&c);
            CloseHandle(c.proc);
            CloseHandle(c.out_read);
            if (c.job) CloseHandle(c.job);
            if (rt) CloseHandle(rt);
            if (wt) CloseHandle(wt);
            CloseHandle(done);
            CloseHandle(ev);
            buf_free(&cap.head);
            buf_free(&cap.tail);
            ExitProcess(2);
        }
        if (left <= 0) {
            st->timed_out = true;
            kill_child(&c);
            WaitForSingleObject(c.proc, 5000);
            break;
        }
    }
    kill_child(&c);   /* 命令留下的后台进程 */
    if (WaitForSingleObject(done, 2000) == WAIT_TIMEOUT && rt) {
        CancelSynchronousIo(rt);
        WaitForSingleObject(done, 1000);
    }
    DWORD code = 0;
    GetExitCodeProcess(c.proc, &code);
    if (!st->timed_out) st->exit_code = (int)code;
    if (c.job) CloseHandle(c.job);
    CloseHandle(c.proc);
    if (c.out_read != INVALID_HANDLE_VALUE) CloseHandle(c.out_read);
    if (rt) CloseHandle(rt);
    if (wt) CloseHandle(wt);
    CloseHandle(done);
    CloseHandle(ev);
    cap_render(&cap, out);
    buf_free(&cap.head);
    buf_free(&cap.tail);
    return 0;
}

int plat_probe(char *const argv[], int timeout_ms, buf_t *out)
{
    child_t c;
    char err[512] = "";
    if (spawn_child(argv, NULL, NULL, false, &c, err, sizeof err) < 0) return -1;
    capture cap = { .head_max = 65536, .tail_max = 0 };
    HANDLE done = CreateEventW(NULL, TRUE, FALSE, NULL);
    reader_arg ra = { .h = c.out_read, .cap = &cap, .done = done };
    HANDLE rt = CreateThread(NULL, 0, reader_thread, &ra, 0, NULL);
    if (WaitForSingleObject(c.proc, (DWORD)timeout_ms) == WAIT_TIMEOUT) kill_child(&c);
    WaitForSingleObject(done, 1000);
    if (c.job) CloseHandle(c.job);
    CloseHandle(c.proc);
    if (c.out_read != INVALID_HANDLE_VALUE) CloseHandle(c.out_read);
    if (rt) CloseHandle(rt);
    CloseHandle(done);
    if (cap.head.len) buf_append(out, cap.head.p, cap.head.len);
    buf_free(&cap.head);
    buf_free(&cap.tail);
    return out->len ? 0 : -1;
}

char *plat_shell_exe(char *const envp[])
{
    char *p = plat_find_in_path(envp, "pwsh.exe");
    if (p) return p;
    p = plat_find_in_path(envp, "powershell.exe");
    return p ? p : xstrdup("powershell.exe");
}

const char *plat_shell_prefix(void)
{
    return "[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false);"
           "$OutputEncoding=[Text.UTF8Encoding]::new($false);";
}

/* ---- 网络、poll、唤醒 ---- */

static void set_wsa_errno(void)
{
    switch (WSAGetLastError()) {
    case WSAEWOULDBLOCK: errno = EAGAIN; break;
    case WSAEINTR: errno = EINTR; break;
    case WSAETIMEDOUT: errno = ETIMEDOUT; break;
    case WSAECONNREFUSED: errno = ECONNREFUSED; break;
    default: errno = EIO; break;
    }
}

int plat_poll(na_pollfd *fds, size_t n, int timeout_ms)
{
    WSAPOLLFD *p = xcalloc(n ? n : 1, sizeof *p);
    for (size_t i = 0; i < n; i++) {
        p[i].fd = (SOCKET)fds[i].id;
        short e = 0;
        if (fds[i].events & NA_POLLIN) e |= POLLRDNORM;
        if (fds[i].events & NA_POLLOUT) e |= POLLWRNORM;
        p[i].events = e;
    }
    int rc = WSAPoll(p, (ULONG)n, timeout_ms);
    if (rc > 0)
        for (size_t i = 0; i < n; i++) {
            short e = 0;
            if (p[i].revents & (POLLRDNORM | POLLIN)) e |= NA_POLLIN;
            if (p[i].revents & POLLWRNORM) e |= NA_POLLOUT;
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
    int r = recv((SOCKET)s, buf, (int)n, 0);
    if (r == 0) return 0;
    if (r == SOCKET_ERROR) { set_wsa_errno(); return -1; }
    return r;
}

long plat_sock_write(na_sock s, const void *buf, size_t n)
{
    int r = send((SOCKET)s, buf, (int)n, 0);
    if (r == SOCKET_ERROR) { set_wsa_errno(); return -1; }
    return r;
}

void plat_sock_close(na_sock s)
{
    if (s != NA_SOCK_NONE) closesocket((SOCKET)s);
}

static int connect_wait(SOCKET s, const struct sockaddr *sa, int len, int timeout_ms)
{
    u_long nb = 1;
    ioctlsocket(s, FIONBIO, &nb);
    if (connect(s, sa, len) == 0) return 0;
    if (WSAGetLastError() != WSAEWOULDBLOCK) return -1;
    WSAPOLLFD p = { .fd = s, .events = POLLOUT };
    int n = WSAPoll(&p, 1, timeout_ms);
    if (n == 0) { WSASetLastError(WSAETIMEDOUT); return -1; }
    if (n < 0) return -1;
    int soerr = 0, sl = sizeof soerr;
    if (getsockopt(s, SOL_SOCKET, SO_ERROR, (char *)&soerr, &sl) < 0) return -1;
    if (soerr) { WSASetLastError(soerr); return -1; }
    return 0;
}

int plat_connect(const char *addr, int timeout_ms, na_sock *out, char *err, size_t errlen)
{
    if (!strncmp(addr, "unix:", 5)) {
        snprintf(err, errlen, "Windows 内核不支持 unix socket，请在 kernel.conf 里用 tcp:主机:端口");
        return -1;
    }
    const char *hostport = !strncmp(addr, "tcp:", 4) ? addr + 4 : addr;
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
        snprintf(err, errlen, "解析 %s 失败：%s", hostport, gai_strerrorA(gai));
        return -1;
    }
    SOCKET s = INVALID_SOCKET;
    int last = 0;
    for (struct addrinfo *ai = res; ai; ai = ai->ai_next) {
        s = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
        if (s == INVALID_SOCKET) { last = WSAGetLastError(); continue; }
        if (connect_wait(s, ai->ai_addr, (int)ai->ai_addrlen, timeout_ms) == 0) break;
        last = WSAGetLastError();
        closesocket(s);
        s = INVALID_SOCKET;
    }
    freeaddrinfo(res);
    if (s == INVALID_SOCKET) {
        WSASetLastError(last);
        set_wsa_errno();
        snprintf(err, errlen, "连接 %s 失败：%s", hostport, strerror(errno));
        return -1;
    }
    int one = 1;
    setsockopt(s, SOL_SOCKET, SO_KEEPALIVE, (const char *)&one, sizeof one);
    *out = (na_sock)s;
    return 0;

bad:
    snprintf(err, errlen, "地址格式不对：%s（应为 host:port）", hostport);
    return -1;
}

int plat_wake_open(na_wake *w)
{
    SOCKET rd = socket(AF_INET, SOCK_DGRAM, 0);
    if (rd == INVALID_SOCKET) return -1;
    struct sockaddr_in a;
    memset(&a, 0, sizeof a);
    a.sin_family = AF_INET;
    a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    a.sin_port = 0;
    if (bind(rd, (struct sockaddr *)&a, sizeof a) < 0) { closesocket(rd); return -1; }
    int al = sizeof a;
    if (getsockname(rd, (struct sockaddr *)&a, &al) < 0) { closesocket(rd); return -1; }
    SOCKET wr = socket(AF_INET, SOCK_DGRAM, 0);
    if (wr == INVALID_SOCKET) { closesocket(rd); return -1; }
    if (connect(wr, (struct sockaddr *)&a, sizeof a) < 0) { closesocket(rd); closesocket(wr); return -1; }
    u_long nb = 1;
    ioctlsocket(rd, FIONBIO, &nb);
    w->rd = (intptr_t)rd;
    w->wr = (intptr_t)wr;
    return 0;
}

void plat_wake_signal(na_wake *w)
{
    if (w->wr < 0) return;
    char c = 1;
    send((SOCKET)w->wr, &c, 1, 0);
}

void plat_wake_drain(na_wake *w)
{
    char buf[64];
    while (recv((SOCKET)w->rd, buf, sizeof buf, 0) > 0) {}
}

void plat_wake_close(na_wake *w)
{
    if (w->rd >= 0) closesocket((SOCKET)w->rd);
    if (w->wr >= 0) closesocket((SOCKET)w->wr);
    w->rd = w->wr = -1;
}

static na_wake *g_swake;
static volatile int *g_sstop;

static BOOL WINAPI on_console_event(DWORD type)
{
    if (type == CTRL_C_EVENT || type == CTRL_BREAK_EVENT || type == CTRL_CLOSE_EVENT ||
        type == CTRL_LOGOFF_EVENT || type == CTRL_SHUTDOWN_EVENT) {
        if (g_sstop) *g_sstop = 1;
        if (g_swake) plat_wake_signal(g_swake);
        return TRUE;
    }
    return FALSE;
}

int plat_signals_install(na_wake *w, volatile int *stop)
{
    g_swake = w;
    g_sstop = stop;
    SetConsoleCtrlHandler(on_console_event, TRUE);
    return 0;
}

void plat_signals_remove(void)
{
    SetConsoleCtrlHandler(on_console_event, FALSE);
}

/* ---- 管理命令用的进程操作 ---- */

long plat_getpid(void) { return (long)GetCurrentProcessId(); }

int plat_log_to(const char *path)
{
    wchar_t *w = to_w(path);
    if (!w) return -1;
    FILE *f = _wfreopen(w, L"ab", stderr);
    free(w);
    if (!f) return -1;
    setvbuf(stderr, NULL, _IONBF, 0);
    SetStdHandle(STD_ERROR_HANDLE, (HANDLE)_get_osfhandle(_fileno(stderr)));
    return 0;
}

bool plat_pid_alive(long pid, const char *name)
{
    if (pid <= 0) return false;
    HANDLE h = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, (DWORD)pid);
    if (!h) return false;
    DWORD code = 0;
    bool alive = GetExitCodeProcess(h, &code) && code == STILL_ACTIVE;
    if (alive && name) {
        wchar_t buf[MAX_PATH * 2];
        DWORD n = MAX_PATH * 2;
        if (QueryFullProcessImageNameW(h, 0, buf, &n)) {
            char *u = to_u8(buf);
            const char *base = u ? strrchr(u, '\\') : NULL;
            alive = u && strstr(base ? base + 1 : u, name) != NULL;
            free(u);
        }
    }
    CloseHandle(h);
    return alive;
}

int plat_pid_kill(long pid, bool force)
{
    (void)force;
    HANDLE h = OpenProcess(PROCESS_TERMINATE, FALSE, (DWORD)pid);
    if (!h) return -1;
    BOOL ok = TerminateProcess(h, 1);
    CloseHandle(h);
    return ok ? 0 : -1;
}

int plat_system(char *const argv[], bool quiet)
{
    wchar_t *line = make_cmdline(argv);
    STARTUPINFOW si;
    ZeroMemory(&si, sizeof si);
    si.cb = sizeof si;
    HANDLE nul = INVALID_HANDLE_VALUE;
    if (quiet) {
        SECURITY_ATTRIBUTES sa = { sizeof sa, NULL, TRUE };
        nul = CreateFileW(L"NUL", GENERIC_READ | GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa,
                          OPEN_EXISTING, 0, NULL);
        si.dwFlags = STARTF_USESTDHANDLES;
        si.hStdInput = GetStdHandle(STD_INPUT_HANDLE);
        si.hStdOutput = nul;
        si.hStdError = nul;
    }
    PROCESS_INFORMATION pi;
    ZeroMemory(&pi, sizeof pi);
    BOOL ok = CreateProcessW(NULL, line, NULL, NULL, TRUE, 0, NULL, NULL, &si, &pi);
    free(line);
    if (nul != INVALID_HANDLE_VALUE) CloseHandle(nul);
    if (!ok) return -1;
    WaitForSingleObject(pi.hProcess, INFINITE);
    DWORD code = 1;
    GetExitCodeProcess(pi.hProcess, &code);
    CloseHandle(pi.hProcess);
    CloseHandle(pi.hThread);
    return (int)code;
}

long plat_spawn_bg(char *const argv[], const char *log)
{
    wchar_t *line = make_cmdline(argv);
    SECURITY_ATTRIBUTES sa = { sizeof sa, NULL, TRUE };
    HANDLE nul = CreateFileW(L"NUL", GENERIC_READ | GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa,
                             OPEN_EXISTING, 0, NULL);
    HANDLE err = INVALID_HANDLE_VALUE;
    if (log) {
        wchar_t *lw = to_w(log);
        err = CreateFileW(lw, FILE_APPEND_DATA, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, &sa,
                          OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
        free(lw);
    }
    STARTUPINFOEXW si;
    ZeroMemory(&si, sizeof si);
    si.StartupInfo.cb = sizeof si;
    si.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    si.StartupInfo.hStdInput = nul;
    si.StartupInfo.hStdOutput = nul;
    si.StartupInfo.hStdError = err != INVALID_HANDLE_VALUE ? err : nul;
    /* 只继承这两个句柄：不然后台进程会拿着调用方终端的输出管道，调用方一直等不到结束 */
    SIZE_T attr_size = 0;
    InitializeProcThreadAttributeList(NULL, 1, 0, &attr_size);
    si.lpAttributeList = xmalloc(attr_size ? attr_size : 1);
    bool have_attr = attr_size && InitializeProcThreadAttributeList(si.lpAttributeList, 1, 0, &attr_size);
    HANDLE keep[2] = { nul, err };
    if (have_attr)
        UpdateProcThreadAttribute(si.lpAttributeList, 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST, keep,
                                  (err != INVALID_HANDLE_VALUE ? 2 : 1) * sizeof(HANDLE), NULL, NULL);
    else
        si.StartupInfo.cb = sizeof(STARTUPINFOW);
    PROCESS_INFORMATION pi;
    ZeroMemory(&pi, sizeof pi);
    /* 不跟着当前终端走：新进程组、没有窗口；当前进程在作业对象里时尽量脱离出来 */
    DWORD flags = CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW | (have_attr ? EXTENDED_STARTUPINFO_PRESENT : 0);
    BOOL ok = CreateProcessW(NULL, line, NULL, NULL, TRUE, flags | CREATE_BREAKAWAY_FROM_JOB, NULL, NULL,
                             &si.StartupInfo, &pi);
    if (!ok) ok = CreateProcessW(NULL, line, NULL, NULL, TRUE, flags, NULL, NULL, &si.StartupInfo, &pi);
    if (have_attr) DeleteProcThreadAttributeList(si.lpAttributeList);
    free(si.lpAttributeList);
    free(line);
    if (nul != INVALID_HANDLE_VALUE) CloseHandle(nul);
    if (err != INVALID_HANDLE_VALUE) CloseHandle(err);
    if (!ok) return -1;
    CloseHandle(pi.hThread);
    CloseHandle(pi.hProcess);
    return (long)pi.dwProcessId;
}
