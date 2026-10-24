/* tools.c —— 内置工具的描述和策略，见 tools.h */
#include "tools.h"

#include <stddef.h>

typedef struct {
    na_tool_policy pol;
    const char *description;
    const char *schema;
} builtin;

/* 每个工具都可以指定在哪个工作目录上执行（会话可以同时挂好几个，可能在不同的机器上） */
#define WS_PROP "\"workspace\":{\"type\":\"string\",\"description\":" \
    "\"在哪个工作目录上执行，写系统消息里列出的名字（机器:路径）。只挂了一个工作目录时可以不写\"}"

/* 提权：要写工作目录外的文件，或命令需要写工作目录外时设为 true；任何模式都要审批 */
#define ELEV_PROP "\"elevated\":{\"type\":\"boolean\",\"description\":" \
    "\"要写工作目录外的文件，或者命令需要写工作目录外时设为 true，会请用户批准\"}"

/* timeout_ms 是内核这边的兜底：执行器自己会先按工具的超时结束 */
static const builtin BUILTINS[] = {
    { { "Schedule", NA_TOOL_SCHEDULE, 70000 }, NULL, NULL },
    { { "WebSearch", NA_TOOL_PUBLIC_READ, 70 * 1000 }, NULL, NULL },
    { { "WebFetch", NA_TOOL_PUBLIC_READ, 70 * 1000 }, NULL, NULL },
    { { "Edit", NA_TOOL_WRITE, 30 * 1000 },
      "把工作目录里的文件中的一段文本精确替换为新文本。old_string 必须与文件内容逐字一致（包括缩进、空格和换行），"
      "并且在文件中只出现一次；要替换所有出现的地方，把 replace_all 设为 true。默认只能写工作目录；写工作目录外要把 elevated 设为 true，会请用户批准。"
      "修改前先用 Read 看过文件，确保 old_string 准确。",
      "{\"type\":\"object\",\"properties\":{"
      "\"path\":{\"type\":\"string\",\"description\":\"文件路径\"},"
      "\"old_string\":{\"type\":\"string\",\"description\":\"要替换的原文\"},"
      "\"new_string\":{\"type\":\"string\",\"description\":\"替换成的新文本\"},"
      "\"replace_all\":{\"type\":\"boolean\",\"description\":\"替换所有出现的地方，默认 false\"}," WS_PROP "," ELEV_PROP "},"
      "\"required\":[\"path\",\"old_string\",\"new_string\"]}" },
    { { "Grep", NA_TOOL_READ, 70 * 1000 },
      "搜索文件内容（正则表达式），结果格式为“文件:行号:内容”。可以搜索文件；"
      "只给 glob 不给 pattern 时，列出文件名匹配的文件。会跳过 .gitignore 里忽略的文件。"
      "有工作目录时相对路径以工作目录为基准，没有工作目录时请写绝对路径或 ~ 开头。找文件、找代码位置时优先用它，而不是 Shell。",
      "{\"type\":\"object\",\"properties\":{"
      "\"pattern\":{\"type\":\"string\",\"description\":\"正则表达式\"},"
      "\"path\":{\"type\":\"string\",\"description\":\"搜索的目录或文件，默认是工作目录\"},"
      "\"glob\":{\"type\":\"string\",\"description\":\"只看文件名匹配的文件，如 *.go\"},"
      "\"ignore_case\":{\"type\":\"boolean\",\"description\":\"忽略大小写\"},"
      "\"files_only\":{\"type\":\"boolean\",\"description\":\"只列出有匹配的文件名\"}," WS_PROP "}}" },
    { { "Read", NA_TOOL_READ, 30 * 1000 },
      "读取文件，返回带行号的内容（行号和内容之间是制表符）。可以读文件；"
      "默认从第 1 行开始最多读 2000 行，单行超过 2000 字节会被截断；大文件用 offset 和 limit 分段读。"
      "路径是目录时列出目录内容。有工作目录时相对路径以工作目录为基准，没有工作目录时请写绝对路径或 ~ 开头。",
      "{\"type\":\"object\",\"properties\":{"
      "\"path\":{\"type\":\"string\",\"description\":\"文件或目录路径\"},"
      "\"offset\":{\"type\":\"integer\",\"description\":\"从第几行开始读，从 1 开始\"},"
      "\"limit\":{\"type\":\"integer\",\"description\":\"最多读多少行\"}," WS_PROP "},"
      "\"required\":[\"path\"]}" },
    { { "Shell", NA_TOOL_SHELL, 610 * 1000 },
      "在工作目录所在的机器上执行一条命令，返回标准输出和标准错误（合在一起）以及非零的退出码。"
      "Linux 和 macOS 上用 /bin/sh，Windows 上用 PowerShell（见系统消息里的环境说明）。"
      "每次调用都是新的 shell，cd 和环境变量不会保留到下一次。命令可以读这台机器上的文件，"
      "默认只能写工作目录和临时目录；需要写工作目录外时设 elevated: true，会请用户批准。命令结束时它启动的后台进程会被清理。不要运行需要交互输入的命令。",
      "{\"type\":\"object\",\"properties\":{"
      "\"command\":{\"type\":\"string\",\"description\":\"要执行的命令\"},"
      "\"timeout\":{\"type\":\"integer\",\"description\":\"超时秒数，默认 120，最大 600\"}," WS_PROP "," ELEV_PROP "},"
      "\"required\":[\"command\"]}" },
    { { "Skill", NA_TOOL_READ, 70 * 1000 }, NULL, NULL },   /* 技能在网关，描述由网关给模型，调用交给网关 */
    { { "Gateway", NA_TOOL_READ, 70 * 1000 }, NULL, NULL }, /* 网关查询：描述由网关给模型 */
    { { "GatewayAdmin", NA_TOOL_ADMIN, 610 * 1000 }, NULL, NULL }, /* 网关修改：任何模式都要审批 */
    { { "Message", NA_TOOL_READ, 320 * 1000 }, NULL, NULL }, /* 给群/别的 agent 发消息：网关执行，不用审批 */
    { { "Write", NA_TOOL_WRITE, 30 * 1000 },
      "创建或覆盖文件，写入完整内容；上级目录不存在会自动创建。默认只能写工作目录；写工作目录外要把 elevated 设为 true，会请用户批准。"
      "只改文件的一部分时用 Edit，不要用 Write 重写整个文件。",
      "{\"type\":\"object\",\"properties\":{"
      "\"path\":{\"type\":\"string\",\"description\":\"文件路径\"},"
      "\"content\":{\"type\":\"string\",\"description\":\"文件的完整内容\"}," WS_PROP "," ELEV_PROP "},"
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
        if (!BUILTINS[i].description) continue;
        cJSON *t = cJSON_CreateObject();
        cJSON_AddStringToObject(t, "name", BUILTINS[i].pol.name);
        cJSON_AddStringToObject(t, "description", BUILTINS[i].description);
        cJSON *schema = cJSON_Parse(BUILTINS[i].schema);
        cJSON_AddItemToObject(t, "input_schema", schema ? schema : cJSON_CreateObject());
        cJSON_AddItemToArray(arr, t);
    }
    return arr;
}
