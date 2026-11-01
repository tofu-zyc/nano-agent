package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var lim = Limits{Record: 1 << 10, File: 1 << 16}

type rec struct {
	N int    `json:"n"`
	S string `json:"s,omitempty"`
}

func open(t *testing.T, path string, version int, l Limits) (*Journal, []rec, error) {
	t.Helper()
	var got []rec
	var last uint64
	j, err := Open(path, version, l, func(seq uint64, data json.RawMessage) error {
		if seq != last+1 {
			t.Fatalf("重放序号 %d 不连续", seq)
		}
		last = seq
		var r rec
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		got = append(got, r)
		return nil
	})
	return j, got, err
}

func mustOpen(t *testing.T, path string) (*Journal, []rec) {
	t.Helper()
	j, got, err := open(t, path, 1, lim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j, got
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAppendReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "j.jsonl")
	j, got := mustOpen(t, path)
	if len(got) != 0 || j.Size() != 0 {
		t.Fatalf("新日志应为空: %v %d", got, j.Size())
	}
	for i := 1; i <= 3; i++ {
		if err := j.Append(rec{N: i, S: "中文<&>"}); err != nil {
			t.Fatal(err)
		}
	}
	size := j.Size()
	j.Close()
	if int64(len(read(t, path))) != size {
		t.Fatalf("Size %d 与文件 %d 不符", size, len(read(t, path)))
	}
	j2, got := mustOpen(t, path)
	if len(got) != 3 || got[2].N != 3 || got[0].S != "中文<&>" || j2.Size() != size {
		t.Fatalf("重放结果不对: %+v %d", got, j2.Size())
	}
	// 重放之后接着追加，序号连续
	if err := j2.Append(rec{N: 4}); err != nil {
		t.Fatal(err)
	}
	j2.Close()
	if _, got := mustOpen(t, path); len(got) != 4 {
		t.Fatalf("续写后应有 4 条: %+v", got)
	}
}

func TestTornTailIsTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := mustOpen(t, path)
	j.Append(rec{N: 1})
	j.Append(rec{N: 2})
	good := j.Size()
	j.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"version":1,"seq":3,"data":{"n`)
	f.Close()
	j2, got := mustOpen(t, path)
	if len(got) != 2 || j2.Size() != good || int64(len(read(t, path))) != good {
		t.Fatalf("半条应被截掉: %+v size=%d file=%d", got, j2.Size(), len(read(t, path)))
	}
	if err := j2.Append(rec{N: 3}); err != nil {
		t.Fatal(err)
	}
	j2.Close()
	if _, got := mustOpen(t, path); len(got) != 3 || got[2].N != 3 {
		t.Fatalf("截断后续写应连上: %+v", got)
	}
}

func TestCorruptionRefusesToOpenAndKeepsFile(t *testing.T) {
	build := func(t *testing.T) (string, []string) {
		path := filepath.Join(t.TempDir(), "j.jsonl")
		j, _ := mustOpen(t, path)
		for i := 1; i <= 3; i++ {
			j.Append(rec{N: i})
		}
		j.Close()
		return path, strings.SplitAfter(strings.TrimSuffix(string(read(t, path)), "\n"), "\n")
	}
	cases := map[string]func(lines []string) []string{
		"中间一条不是 JSON": func(l []string) []string { l[1] = "garbage\n"; return l },
		"数据被改":        func(l []string) []string { l[1] = strings.Replace(l[1], `"n":2`, `"n":9`, 1); return l },
		"序号跳号":        func(l []string) []string { return []string{l[0], l[2]} },
		"序号重复":        func(l []string) []string { return []string{l[0], l[0], l[2]} },
		"版本不支持":       func(l []string) []string { l[1] = strings.Replace(l[1], `"version":1`, `"version":2`, 1); return l },
		"最后一条完整但损坏":   func(l []string) []string { l[2] = "{\"version\":1}\n"; return l },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path, lines := build(t)
			for i := range lines {
				if !strings.HasSuffix(lines[i], "\n") {
					lines[i] += "\n"
				}
			}
			bad := strings.Join(mutate(lines), "")
			if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := open(t, path, 1, lim); err == nil {
				t.Fatal("损坏的日志不该打开")
			}
			if string(read(t, path)) != bad {
				t.Fatal("拒绝打开时文件必须保持原样")
			}
		})
	}
}

func TestCorruptCompleteRecordBeforeTornTailKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	body := "garbage\n" + `{"version":1,"seq":2,"da`
	os.WriteFile(path, []byte(body), 0o600)
	if _, _, err := open(t, path, 1, lim); err == nil {
		t.Fatal("应拒绝")
	}
	if string(read(t, path)) != body {
		t.Fatal("文件被改动")
	}
}

func TestReplayErrorRefusesAndKeepsTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := mustOpen(t, path)
	j.Append(rec{N: 1})
	j.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"ver`)
	f.Close()
	before := read(t, path)
	boom := errors.New("业务校验失败")
	_, err := Open(path, 1, lim, func(uint64, json.RawMessage) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("应带回 replay 的错误: %v", err)
	}
	if string(read(t, path)) != string(before) {
		t.Fatal("replay 失败时不该截断文件")
	}
}

func TestLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _, err := open(t, path, 1, Limits{Record: 64, File: 400})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.Append(rec{S: strings.Repeat("x", 100)}); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("单条超限: %v", err)
	}
	if j.Size() != 0 {
		t.Fatal("超限的记录不该落盘")
	}
	var full error
	for i := 0; i < 20 && full == nil; i++ {
		full = j.Append(rec{N: i})
	}
	if !errors.Is(full, ErrFileFull) {
		t.Fatalf("文件超限: %v", full)
	}
	if j.Size() > 400 || int64(len(read(t, path))) != j.Size() {
		t.Fatalf("文件不该超过上限: %d", j.Size())
	}
	// 超限不冻结：改写后还能继续用
	if err := j.Rewrite([]any{rec{N: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(rec{N: 2}); err != nil {
		t.Fatalf("超限不应冻结: %v", err)
	}
	// 打开时文件已超上限
	j.Close()
	if _, _, err := open(t, path, 1, Limits{Record: 64, File: 10}); !errors.Is(err, ErrFileFull) {
		t.Fatalf("已有文件超限应拒绝: %v", err)
	}
	// 重放里的记录超过单条上限
	if _, _, err := open(t, path, 1, Limits{Record: 4, File: 400}); err == nil {
		t.Fatal("记录超过单条上限应拒绝")
	}
}

func TestRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := mustOpen(t, path)
	for i := 1; i <= 5; i++ {
		j.Append(rec{N: i})
	}
	if err := j.Rewrite([]any{rec{N: 4}, rec{N: 5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".next"); !os.IsNotExist(err) {
		t.Fatal(".next 应已被替换掉")
	}
	if int64(len(read(t, path))) != j.Size() {
		t.Fatalf("Size %d 与文件 %d 不符", j.Size(), len(read(t, path)))
	}
	if err := j.Append(rec{N: 6}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	_, got := mustOpen(t, path)
	if len(got) != 3 || got[0].N != 4 || got[2].N != 6 {
		t.Fatalf("改写后序号应从 1 重排并能续写: %+v", got)
	}
}

func TestRewriteTooLargeLeavesOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _, _ := open(t, path, 1, Limits{Record: 64, File: 300})
	defer j.Close()
	j.Append(rec{N: 1})
	before := read(t, path)
	many := make([]any, 10)
	for i := range many {
		many[i] = rec{N: i}
	}
	if err := j.Rewrite(many); !errors.Is(err, ErrFileFull) {
		t.Fatalf("改写超限: %v", err)
	}
	if err := j.Rewrite([]any{rec{S: strings.Repeat("x", 100)}}); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("改写单条超限: %v", err)
	}
	if string(read(t, path)) != string(before) {
		t.Fatal("改写失败不该动原文件")
	}
	if _, err := os.Stat(path + ".next"); !os.IsNotExist(err) {
		t.Fatal("不该留下 .next")
	}
	if err := j.Append(rec{N: 2}); err != nil {
		t.Fatalf("改写被拒后仍可追加: %v", err)
	}
}

func TestInterruptedRewriteReadsOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := mustOpen(t, path)
	j.Append(rec{N: 1})
	j.Append(rec{N: 2})
	j.Close()
	// 写 .next 写到一半就中断：旧文件仍是权威
	os.WriteFile(path+".next", []byte(`{"version":1,"seq":1,"data":{"n":9`), 0o600)
	_, got := mustOpen(t, path)
	if len(got) != 2 || got[0].N != 1 {
		t.Fatalf("应读到旧文件: %+v", got)
	}
	if _, err := os.Stat(path + ".next"); !os.IsNotExist(err) {
		t.Fatal("打开时应清掉残留的 .next")
	}
}

func TestRewriteReplaceFailureFreezesAndOldFileSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := mustOpen(t, path)
	j.Append(rec{N: 1})
	j.Append(rec{N: 2})
	old := replaceFile
	replaceFile = func(string, string) error { return errors.New("磁盘错误") }
	defer func() { replaceFile = old }()
	if err := j.Rewrite([]any{rec{N: 9}}); err == nil {
		t.Fatal("替换失败应报错")
	}
	if err := j.Append(rec{N: 3}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("替换失败后应冻结: %v", err)
	}
	if err := j.Rewrite(nil); !errors.Is(err, ErrFrozen) {
		t.Fatalf("冻结后拒绝改写: %v", err)
	}
	j.Close()
	replaceFile = old
	if _, got := mustOpen(t, path); len(got) != 2 || got[1].N != 2 {
		t.Fatalf("重启后读到旧文件: %+v", got)
	}
}

func TestFrozenAfterWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _ := mustOpen(t, path)
	j.Append(rec{N: 1})
	// 底层文件被关掉：下一次写入失败
	j.file.Close()
	err := j.Append(rec{N: 2})
	if err == nil || errors.Is(err, ErrFrozen) {
		t.Fatalf("第一次失败应返回底层错误: %v", err)
	}
	if err := j.Append(rec{N: 3}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("之后都应是冻结: %v", err)
	}
	if err := j.Append(rec{N: 4}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("一直冻结: %v", err)
	}
	if _, got := mustOpen(t, path); len(got) != 1 {
		t.Fatalf("重启只剩已提交的: %+v", got)
	}
}

func TestVersionAndArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.jsonl")
	j, _, _ := open(t, path, 2, lim)
	j.Append(rec{N: 1})
	j.Close()
	if _, _, err := open(t, path, 1, lim); err == nil {
		t.Fatal("版本不同应拒绝")
	}
	if _, _, err := open(t, path, 0, lim); err == nil {
		t.Fatal("版本要大于 0")
	}
	if _, _, err := open(t, path, 2, Limits{}); err == nil {
		t.Fatal("上限要大于 0")
	}
	if err := (&Journal{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUseAfterCloseReturnsError(t *testing.T) {
	j, _ := mustOpen(t, filepath.Join(t.TempDir(), "j.jsonl"))
	j.Append(rec{N: 1})
	j.Close()
	if err := j.Append(rec{N: 2}); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后追加: %v", err)
	}
	if err := j.Rewrite([]any{rec{N: 2}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后改写: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("重复关闭: %v", err)
	}
	// 冻结之后关闭再用，仍然是冻结的错误而不是崩溃
	k, _ := mustOpen(t, filepath.Join(t.TempDir(), "k.jsonl"))
	k.file.Close()
	k.Append(rec{N: 1})
	k.Close()
	if err := k.Append(rec{N: 2}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("冻结后关闭再追加: %v", err)
	}
}

func TestUnserializableRecordIsRejectedWithoutFreezing(t *testing.T) {
	j, _ := mustOpen(t, filepath.Join(t.TempDir(), "j.jsonl"))
	if err := j.Append(make(chan int)); err == nil {
		t.Fatal("不能序列化的数据应报错")
	}
	if err := j.Append(rec{N: 1}); err != nil {
		t.Fatalf("序列化失败不该冻结: %v", err)
	}
}
