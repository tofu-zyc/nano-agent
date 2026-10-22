/* tools.h —— 内置工具：Read、Write、Edit、Grep、Shell，以及 Skill
 *
 * 工具集是固定的：描述原样报给网关交给模型（不变的工具定义对前缀缓存最友好），
 * 执行在 nano-exec 里完成（见 nexec.h）。技能放在网关上，Skill 的描述由网关给模型，
 * 调用也交给网关，这里只有它的策略。
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
