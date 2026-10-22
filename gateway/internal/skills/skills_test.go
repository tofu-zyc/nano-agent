package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

func TestScanLoadAndFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "demo", "SKILL.md"), "---\nname: demo\ndescription: >\n  示例技能，\n  测试用\n---\n\n# 示例\n照着做。\n", 0o644)
	write(t, filepath.Join(dir, "demo", "scripts", "run.sh"), "echo hi\n", 0o755)
	write(t, filepath.Join(dir, "demo", "a", "b", "c", "deep.txt"), "太深了", 0o644)
	os.Symlink("/etc/passwd", filepath.Join(dir, "demo", "link"))
	write(t, filepath.Join(dir, "noname", "SKILL.md"), "没有 frontmatter", 0o644)
	os.MkdirAll(filepath.Join(dir, "empty"), 0o755) // 没有 SKILL.md 的目录不算技能

	r := New(dir)
	if !r.Scan() || r.Scan() {
		t.Fatal("第一次扫描有变化，第二次没有")
	}
	list := r.List()
	if len(list) != 2 || list[0].Name != "demo" || list[0].Description != "示例技能， 测试用" || list[1].Name != "noname" || list[1].Description != "（没有简介）" {
		t.Fatalf("list: %+v", list)
	}
	spec, ok := r.ToolSpec()
	if !ok || spec.Name != "Skill" || !strings.Contains(spec.Description, "\n- demo：示例技能， 测试用") || !strings.Contains(string(spec.InputSchema), `"enum":["demo","noname"]`) {
		t.Fatalf("spec: %+v %s", spec, spec.InputSchema)
	}
	text, files, hash, ok := r.Load("demo")
	if !ok || hash == "" || !strings.HasPrefix(text, "技能 demo\n技能目录：~/.cache/nano-agent/skills/demo") ||
		!strings.Contains(text, "附带文件：scripts/run.sh\n") || !strings.HasSuffix(text, "# 示例\n照着做。\n") {
		t.Fatalf("text: %q", text)
	}
	if len(files) != 2 || files[0].Path != "SKILL.md" || files[1].Path != "scripts/run.sh" || !files[1].Exec {
		t.Fatalf("files: %+v", files) // 太深的和符号链接不带
	}
	if text, _, _, ok := r.Load("nope"); ok || !strings.Contains(text, "没有这个技能") {
		t.Fatalf("nope: %q", text)
	}
	os.RemoveAll(filepath.Join(dir, "noname"))
	if !r.Scan() || len(r.List()) != 1 {
		t.Fatal("删掉技能后应该有变化")
	}
}
