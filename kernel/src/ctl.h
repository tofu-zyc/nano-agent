/* ctl.h —— nano-kernel 的管理命令（status / start / stop / connect …），见 ctl.c */
#ifndef NA_CTL_H
#define NA_CTL_H

#include <stdbool.h>
#include <stdio.h>

bool ctl_is_command(const char *s);
int  ctl_main(int argc, char **argv);
void ctl_usage(FILE *out);
/* 配置文件旁边的状态文件：kernel.conf → kernel.pid / kernel.status.json / kernel.stop（malloc） */
char *ctl_sidecar(const char *conf, const char *ext);

#endif
