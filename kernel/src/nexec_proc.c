/* nexec_proc.c —— Shell 和 Grep（具体怎么起子进程在 plat 里） */
#include "nexec.h"
#include "plat.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void put_status(buf_t *out, const proc_status *st, int timeout_s)
{
    char note[96] = "";
    if (st->timed_out) snprintf(note, sizeof note, "（执行超时（%d 秒），已终止）", timeout_s);
    else if (st->term_signal) snprintf(note, sizeof note, "（被信号 %d 终止）", st->term_signal);
    else if (st->exit_code) snprintf(note, sizeof note, "（退出码 %d）", st->exit_code);
    if (!*note) return;
    if (out->len && out->p[out->len - 1] != '\n') buf_puts(out, "\n");
    buf_puts(out, note);
}

/* 发布包里 rg 和执行器放在一起，优先用它；没有再用 PATH 里的 */
char *rg_path(const xctx *x)
{
    char *dir = plat_exe_dir();
    char *p = NULL;
    if (dir) {
        p = plat_join(dir, NA_WINDOWS ? "rg.exe" : "rg");
        free(dir);
        if (plat_is_executable(p)) return p;
        free(p);
    }
    return plat_find_in_path(x->env, NA_WINDOWS ? "rg.exe" : "rg");
}

/* ---- Shell ---- */

bool tool_shell(const xctx *x, const cJSON *a, buf_t *out)
{
    const cJSON *cmd = cJSON_GetObjectItemCaseSensitive(a, "command");
    const cJSON *to = cJSON_GetObjectItemCaseSensitive(a, "timeout");
    if (!cJSON_IsString(cmd) || !*cmd->valuestring) {
        buf_puts(out, "缺少 command（字符串）");
        return false;
    }
    if (!x->ndirs && !x->home_only) {
        buf_puts(out, "还没有设置工作目录，Shell 没有地方运行。请让用户先用 /dir add <路径> 添加工作目录");
        return false;
    }
    int timeout = cJSON_IsNumber(to) && to->valuedouble>=0 && to->valuedouble<=86400*365
                  ? (int)to->valuedouble : x->stream ? 1800 : SHELL_DEFAULT_TIMEOUT;
    if (!x->stream && cJSON_IsNumber(to)) {
        timeout=to->valuedouble<1?1:to->valuedouble>SHELL_MAX_TIMEOUT?SHELL_MAX_TIMEOUT:(int)to->valuedouble;
    }
    int maximum=x->stream ? (x->shell_timeout_max>0?x->shell_timeout_max:86400) : SHELL_MAX_TIMEOUT;
    if (x->stream && timeout==0 && !x->allow_no_timeout) {
        buf_puts(out,"执行节点未允许不限时任务");return false;
    }
    if (!x->stream && timeout<1) timeout=1;
    if (timeout>maximum) timeout=maximum;

    char *exe = plat_shell_exe(x->env);
    char *wrapped = NULL;
    char *argv[8];
    int n = 0;
    argv[n++] = exe;
#if NA_WINDOWS
    argv[n++] = "-NoProfile";
    argv[n++] = "-NonInteractive";
    argv[n++] = "-Command";
    const char *prefix = plat_shell_prefix();
    wrapped = xasprintf("%s%s", prefix ? prefix : "", cmd->valuestring);
    argv[n++] = wrapped;
#else
    argv[n++] = "-c";
    argv[n++] = cmd->valuestring;
#endif
    argv[n] = NULL;
    proc_status st;
    proc_stream input_stream = x->stream ? *x->stream : (proc_stream){0};
    input_stream.input=x->hook_input;
    const proc_stream *proc=x->hook_input ? &input_stream : x->stream;
    int rc = plat_run_proc_stream(argv, x->ndirs ? x->dirs[0] : x->home, x->env, timeout,
                           x->watch_stdin, x->max_output, out, &st, proc);
    if (x->process_status) *x->process_status=st;
    free(exe);
    free(wrapped);
    if (rc < 0) return false;
    if (!out->len) buf_puts(out, "（无输出）");
    put_status(out, &st, timeout);
    return !st.cancelled && !st.timed_out && st.term_signal == 0 && st.exit_code == 0;
}

/* ---- Grep ---- */

/* 有 ripgrep 就用它（自动跳过 .gitignore 里的文件），否则退回 grep / find */
bool tool_grep(const xctx *x, const cJSON *a, buf_t *out)
{
    const cJSON *pv = cJSON_GetObjectItemCaseSensitive(a, "pattern");
    const cJSON *gv = cJSON_GetObjectItemCaseSensitive(a, "glob");
    const cJSON *pathv = cJSON_GetObjectItemCaseSensitive(a, "path");
    const char *pattern = cJSON_IsString(pv) && *pv->valuestring ? pv->valuestring : NULL;
    const char *glob = cJSON_IsString(gv) && *gv->valuestring ? gv->valuestring : NULL;
    bool icase = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(a, "ignore_case"));
    bool files = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(a, "files_only"));
    if (!pattern && !glob) {
        buf_puts(out, "pattern 和 glob 至少要给一个");
        return false;
    }
    const char *given = cJSON_IsString(pathv) && *pathv->valuestring ? pathv->valuestring : ".";
    char *real = path_resolve(x, given, out);
    if (!real) return false;
    bool ok = false;
    char *rg = NULL, *tool = NULL, *globarg = NULL, *rel = NULL;
    plat_stat_t st;
    if (!path_can_read(x, real, out)) goto done;
    if (!plat_stat(real, &st)) {
        buf_puts(out, "路径不存在：");
        buf_puts(out, real);
        goto done;
    }
    /* 在默认工作目录下执行，结果里的路径尽量是相对路径，短一些 */
    const char *cwd = x->ndirs ? x->dirs[0] : x->home;
    const char *target = real;
    rel = plat_relpath(real, cwd);
    if (rel) target = rel;

    char *argv[24];
    int n = 0;
    rg = rg_path(x);
    if (rg) {
        argv[n++] = rg;
        if (!pattern) {
            argv[n++] = "--files";
        } else {
            argv[n++] = "--no-heading";
            argv[n++] = "--line-number";
            argv[n++] = "--color=never";
            argv[n++] = "--max-columns=500";
            if (icase) argv[n++] = "--ignore-case";
            if (files) argv[n++] = "--files-with-matches";
        }
        if (glob) { argv[n++] = "--glob"; argv[n++] = (char *)glob; }
        if (pattern) { argv[n++] = "--regexp"; argv[n++] = (char *)pattern; }
        argv[n++] = "--";
        argv[n++] = (char *)target;
    } else if (pattern) {
#if NA_WINDOWS
        buf_puts(out, "这台机器上没有 rg。发布包会把 rg.exe 放在执行器旁边；从源码构建时把它复制到 nano-exec.exe 旁边即可");
        goto done;
#else
        tool = plat_find_in_path(x->env, "grep");
        if (!tool) { buf_puts(out, "这台机器上既没有 rg 也没有 grep"); goto done; }
        argv[n++] = tool;
        argv[n++] = "-rnIE";
        argv[n++] = "--exclude-dir=.git";
        if (icase) argv[n++] = "-i";
        if (files) argv[n++] = "-l";
        if (glob) { globarg = xasprintf("--include=%s", glob); argv[n++] = globarg; }
        argv[n++] = "-e";
        argv[n++] = (char *)pattern;
        argv[n++] = "--";
        argv[n++] = (char *)target;
#endif
    } else {
#if NA_WINDOWS
        buf_puts(out, "这台机器上没有 rg。发布包会把 rg.exe 放在执行器旁边；从源码构建时把它复制到 nano-exec.exe 旁边即可");
        goto done;
#else
        tool = plat_find_in_path(x->env, "find");
        if (!tool) { buf_puts(out, "这台机器上既没有 rg 也没有 find"); goto done; }
        argv[n++] = tool;
        argv[n++] = (char *)target;
        argv[n++] = "-type";
        argv[n++] = "f";
        argv[n++] = "-name";
        argv[n++] = (char *)glob;
        argv[n++] = "-not";
        argv[n++] = "-path";
        argv[n++] = "*/.git/*";
#endif
    }
    argv[n] = NULL;

    proc_status ps;
    if (plat_run_proc(argv, cwd, x->env, GREP_TIMEOUT, x->watch_stdin, x->max_output, out, &ps) < 0) goto done;
    if (ps.timed_out) {
        put_status(out, &ps, GREP_TIMEOUT);
    } else if (ps.term_signal == 0 && ps.exit_code == 1 && !out->len) {
        buf_puts(out, "没有找到匹配");
        ok = true;
    } else if (ps.term_signal == 0 && ps.exit_code == 0) {
        if (!out->len) buf_puts(out, "没有找到匹配");
        ok = true;
    } else {
        put_status(out, &ps, GREP_TIMEOUT);
    }
done:
    free(real);
    free(rg);
    free(tool);
    free(globarg);
    free(rel);
    return ok;
}
