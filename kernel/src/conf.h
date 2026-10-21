/* conf.h —— 内核配置（kernel.conf，每行 key = value，# 开头为注释） */
#ifndef NA_CONF_H
#define NA_CONF_H

#include <stddef.h>

/* 通过 SSH 执行工具的服务器（服务器上不跑内核，每次调用临时跑一下 nano-exec） */
typedef struct {
    char *name;    /* 节点名，网关和会话设置里用它 */
    char *target;  /* 交给 ssh 的目标：~/.ssh/config 里的别名，或者 user@host */
    char *exec;    /* 服务器上 nano-exec 的路径；NULL 表示自动上传 */
} kremote;

typedef struct {
    char  *gateway;           /* unix:/path 或 tcp:host:port */
    char  *token;             /* 与网关共享的口令 */
    char  *node;              /* 本内核的名字，默认主机名 */
    char  *exec;              /* 执行器 nano-exec 的路径，默认和内核放在同一个目录 */
    char  *dir;               /* 配置文件所在目录（解析过符号链接），不能被用作工作目录 */
    int    max_steps;         /* 每个 run 最多请求几次模型 */
    int    run_timeout;       /* 秒 */
    int    approval_timeout;  /* 秒 */
    size_t max_output;        /* 工具输出最多保留的字节数 */
    char  *ssh;               /* ssh 命令，默认 ssh */
    char  *skills;            /* 技能目录，默认是配置目录下的 skills/ */
    kremote *remotes;         /* remote = 名字 目标 [服务器上 nano-exec 的路径] */
    size_t nremotes;
} kconf;

int  kconf_load(kconf *k, const char *path, char *err, size_t errlen);
void kconf_free(kconf *k);

#endif
