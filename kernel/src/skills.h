/* skills.h —— 技能（Anthropic Agent Skills 格式）
 *
 * 技能目录（kernel.conf 的 skills，默认是配置目录下的 skills/）里每个子目录是一个技能：
 *   skills/pdf/SKILL.md        开头是 frontmatter（name、description），后面是说明
 *   skills/pdf/scripts/…       附带的脚本、资料，说明里会提到
 * 内核把技能的名字和简介写进 Skill 工具的描述（技能不变时描述不变，不破坏前缀缓存）；
 * 模型调用 Skill 时返回 SKILL.md 的正文和附带文件的位置。技能目录在沙箱里只读可见，
 * 附带的文件用 Read 读、用 Shell 运行。
 */
#ifndef NA_SKILLS_H
#define NA_SKILLS_H

#include "cJSON.h"

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct {
    char *name, *description, *dir;
} skill_t;

typedef struct {
    char    *root;     /* 技能目录（解析过符号链接），不存在时为 NULL */
    skill_t *list;     /* 按名字排序 */
    size_t   n;
    uint64_t sig;      /* 技能列表的指纹，用来判断有没有变 */
} skills_t;

/* 重新扫描技能目录，返回列表有没有变 */
bool   skills_scan(skills_t *s, const char *root);
void   skills_free(skills_t *s);
/* Skill 工具的定义 {name, description, input_schema}；没有技能时返回 NULL */
cJSON *skills_tool_spec(const skills_t *s);
/* [{name, description}]，hello 里报给网关 */
cJSON *skills_json(const skills_t *s);
const skill_t *skills_find(const skills_t *s, const char *name);
/* 服务器上的技能缓存根目录（~ 开头），按内核和技能目录区分 */
char  *skills_remote_root(const skills_t *s, const char *node);
/* 技能的内容（工具结果）；dir 非空时使用服务器上的目录。失败时 *ok 为 false */
char  *skill_load(const skills_t *s, const char *name, const char *dir, bool *ok);

#endif
