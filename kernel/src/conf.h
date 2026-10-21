/* conf.h —— 内核配置（kernel.conf，每行 key = value，# 开头为注释） */
#ifndef NA_CONF_H
#define NA_CONF_H

#include <stddef.h>

typedef struct {
    char  *gateway;           /* unix:/path 或 tcp:host:port */
    char  *token;             /* 与网关共享的口令 */
    char  *node;              /* 本内核的名字，默认主机名 */
    char  *tools_dir;         /* 工具目录，相对路径以配置文件所在目录为基准 */
    int    max_steps;         /* 每个 run 最多请求几次模型 */
    int    run_timeout;       /* 秒 */
    int    approval_timeout;  /* 秒 */
    size_t max_output;        /* 工具输出最多保留的字节数 */
    char **admins;
    size_t nadmins;
} kconf;

int  kconf_load(kconf *k, const char *path, char *err, size_t errlen);
void kconf_free(kconf *k);

#endif
