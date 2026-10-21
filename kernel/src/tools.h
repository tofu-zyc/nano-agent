/* tools.h —— 内置工具：Read、Write、Edit、Grep、Shell
 *
 * 工具集是固定的：描述原样报给网关交给模型（不变的工具定义对前缀缓存最友好），
 * 执行在 nano-exec 里完成（见 nexec.h）。
 */
#ifndef NA_TOOLS_H
#define NA_TOOLS_H

#include "cJSON.h"
#include "core.h"

/* 内核用的策略，按名字排序 */
const na_tool_policy *tools_policies(size_t *n);

/* 给模型看的工具定义：[{name, description, input_schema}]，按名字排序 */
cJSON *tools_specs(void);

#endif
