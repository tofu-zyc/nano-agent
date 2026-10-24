/* conf.h —— 内核配置（kernel.conf，每行 key = value，# 开头为注释） */
#ifndef NA_CONF_H
#define NA_CONF_H

#include <stdbool.h>
#include <stddef.h>

typedef struct {
    char  *gateway;           /* unix:/path 或 tcp:host:port */
    char  *token;             /* 与网关共享的口令；没有时向网关请求配对（空字符串） */
    char  *token_file;        /* 配对得到的口令存在这里：配置目录下的 paired.token */
    bool   paired;            /* token 是配对得到的（网关不认了就删掉，重新配对） */
    char  *node;              /* 本内核的名字，默认主机名 */
    char  *exec;              /* 执行器 nano-exec 的路径，默认和内核放在同一个目录 */
    char  *dir;               /* 配置文件所在目录（解析过符号链接），不能被用作工作目录 */
    /* 下面四项只是兜底：网关每一轮都带上自己的设置（拓扑图里改），以网关为准 */
    int    max_steps;         /* 每个 run 最多请求几次模型 */
    int    run_timeout;       /* 秒 */
    int    approval_timeout;  /* 秒 */
    size_t max_output;        /* 工具输出最多保留的字节数 */
    char  *skills;            /* 旧配置：技能目录（技能已经改放在网关上，只用来提示） */
} kconf;

int  kconf_load(kconf *k, const char *path, char *err, size_t errlen);
/* 配置项的名字（以 NULL 结尾） */
extern const char *const KCONF_KEYS[];
/* 值里夹着另一个配置项（两行粘在了一起）：返回第二项开始的位置，没有返回 NULL */
const char *kconf_glued(const char *val);
void kconf_free(kconf *k);

#endif
