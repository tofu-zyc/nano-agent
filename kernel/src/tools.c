/* tools.c —— 内置工具的描述和策略，见 tools.h */
#include "tools.h"

#include <stddef.h>

typedef struct {
    na_tool_policy pol;
    const char *description;
    const char *schema;
} builtin;

/* timeout_ms 是内核这边的兜底：执行器自己会先按工具的超时结束 */
static const builtin BUILTINS[] = {
    { { "Edit", NA_TOOL_WRITE, 30 * 1000 },
      "把文件中的一段文本精确替换为新文本。old_string 必须与文件内容逐字一致（包括缩进、空格和换行），"
      "并且在文件中只出现一次；要替换所有出现的地方，把 replace_all 设为 true。"
      "修改前先用 Read 看过文件，确保 old_string 准确。",
      "{\"type\":\"object\",\"properties\":{"
      "\"path\":{\"type\":\"string\",\"description\":\"文件路径\"},"
      "\"old_string\":{\"type\":\"string\",\"description\":\"要替换的原文\"},"
      "\"new_string\":{\"type\":\"string\",\"description\":\"替换成的新文本\"},"
      "\"replace_all\":{\"type\":\"boolean\",\"description\":\"替换所有出现的地方，默认 false\"}},"
      "\"required\":[\"path\",\"old_string\",\"new_string\"]}" },
    { { "Grep", NA_TOOL_READ, 70 * 1000 },
      "在工作目录里搜索文件内容（正则表达式），结果格式为“文件:行号:内容”。"
      "只给 glob 不给 pattern 时，列出文件名匹配的文件。会跳过 .gitignore 里忽略的文件。"
      "找文件、找代码位置时优先用它，而不是 Shell。",
      "{\"type\":\"object\",\"properties\":{"
      "\"pattern\":{\"type\":\"string\",\"description\":\"正则表达式\"},"
      "\"path\":{\"type\":\"string\",\"description\":\"搜索的目录或文件，默认是默认工作目录\"},"
      "\"glob\":{\"type\":\"string\",\"description\":\"只看文件名匹配的文件，如 *.go\"},"
      "\"ignore_case\":{\"type\":\"boolean\",\"description\":\"忽略大小写\"},"
      "\"files_only\":{\"type\":\"boolean\",\"description\":\"只列出有匹配的文件名\"}}}" },
    { { "Read", NA_TOOL_READ, 30 * 1000 },
      "读取工作目录里的文件，返回带行号的内容（行号和内容之间是制表符）。默认从第 1 行开始最多读 2000 行，"
      "单行超过 2000 字节会被截断；大文件用 offset 和 limit 分段读。路径是目录时列出目录内容。"
      "相对路径以默认工作目录为基准。",
      "{\"type\":\"object\",\"properties\":{"
      "\"path\":{\"type\":\"string\",\"description\":\"文件或目录路径\"},"
      "\"offset\":{\"type\":\"integer\",\"description\":\"从第几行开始读，从 1 开始\"},"
      "\"limit\":{\"type\":\"integer\",\"description\":\"最多读多少行\"}},"
      "\"required\":[\"path\"]}" },
    { { "Shell", NA_TOOL_SHELL, 610 * 1000 },
      "在默认工作目录下用 /bin/sh 执行一条命令，返回标准输出和标准错误（合在一起）以及非零的退出码。"
      "每次调用都是新的 shell，cd 和环境变量不会保留到下一次。命令只能读写工作目录和临时目录（$TMPDIR），"
      "系统目录只读，HOME 等其他位置不可访问；命令结束时它启动的后台进程会被清理。"
      "不要运行需要交互输入的命令。",
      "{\"type\":\"object\",\"properties\":{"
      "\"command\":{\"type\":\"string\",\"description\":\"要执行的命令\"},"
      "\"timeout\":{\"type\":\"integer\",\"description\":\"超时秒数，默认 120，最大 600\"}},"
      "\"required\":[\"command\"]}" },
    { { "Write", NA_TOOL_WRITE, 30 * 1000 },
      "创建或覆盖工作目录里的文件，写入完整内容；上级目录不存在会自动创建。"
      "只改文件的一部分时用 Edit，不要用 Write 重写整个文件。",
      "{\"type\":\"object\",\"properties\":{"
      "\"path\":{\"type\":\"string\",\"description\":\"文件路径\"},"
      "\"content\":{\"type\":\"string\",\"description\":\"文件的完整内容\"}},"
      "\"required\":[\"path\",\"content\"]}" },
};

#define NBUILTINS (sizeof BUILTINS / sizeof BUILTINS[0])

const na_tool_policy *tools_policies(size_t *n)
{
    static na_tool_policy pols[NBUILTINS];
    for (size_t i = 0; i < NBUILTINS; i++) pols[i] = BUILTINS[i].pol;
    *n = NBUILTINS;
    return pols;
}

cJSON *tools_specs(void)
{
    cJSON *arr = cJSON_CreateArray();
    for (size_t i = 0; i < NBUILTINS; i++) {
        cJSON *t = cJSON_CreateObject();
        cJSON_AddStringToObject(t, "name", BUILTINS[i].pol.name);
        cJSON_AddStringToObject(t, "description", BUILTINS[i].description);
        cJSON *schema = cJSON_Parse(BUILTINS[i].schema);
        cJSON_AddItemToObject(t, "input_schema", schema ? schema : cJSON_CreateObject());
        cJSON_AddItemToArray(arr, t);
    }
    return arr;
}
