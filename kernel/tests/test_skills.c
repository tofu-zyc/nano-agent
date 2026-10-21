/* 技能的真实目录扫描：正文更新不改变模型前缀，元数据和删除会更新工具。 */
#include "skills.h"
#include "util.h"

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

static void put(const char *path, const char *text)
{
    FILE *f = fopen(path, "w");
    assert(f);
    assert(fputs(text, f) >= 0);
    assert(fclose(f) == 0);
}

int main(void)
{
    char root[] = "/tmp/na-skills-XXXXXX";
    assert(mkdtemp(root));
    char *dir = xasprintf("%s/demo", root);
    char *md = xasprintf("%s/SKILL.md", dir);
    assert(mkdir(dir, 0700) == 0);
    put(md, "---\nname: demo\ndescription: '示例技能'\n---\n第一版正文\n");
    skills_t s = {0};
    assert(skills_scan(&s, root));
    assert(s.n == 1);
    cJSON *spec = skills_tool_spec(&s);
    char *before = cJSON_PrintUnformatted(spec);
    cJSON_Delete(spec);
    assert(!skills_scan(&s, root));

    put(md, "---\nname: demo\ndescription: '示例技能'\n---\n第二版正文\n");
    assert(!skills_scan(&s, root));
    spec = skills_tool_spec(&s);
    char *after = cJSON_PrintUnformatted(spec);
    cJSON_Delete(spec);
    assert(!strcmp(before, after));
    bool ok;
    char *body = skill_load(&s, "demo", NULL, &ok);
    assert(ok && strstr(body, "第二版正文") && !strstr(body, "description:"));
    free(body);
    cJSON_free(before);
    cJSON_free(after);

    put(md, "---\nname: demo\ndescription: |\n  新简介\n  第二行\n---\n正文\n");
    assert(skills_scan(&s, root));
    assert(!strcmp(s.list[0].description, "新简介\n第二行"));
    assert(unlink(md) == 0);
    assert(skills_scan(&s, root));
    assert(s.n == 0 && !skills_tool_spec(&s));
    skills_free(&s);
    assert(rmdir(dir) == 0);
    assert(rmdir(root) == 0);
    free(md);
    free(dir);
    puts("skills: scan, reload, stable prefix and removal passed");
    return 0;
}
