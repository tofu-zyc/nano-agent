package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupCommittedCleanupFailure 产生真实的“改名已提交、源清理失败”现场。
// 返回的操作记录必须是 committed，而不是手工写出来的。
func setupCommittedCleanupFailure(t *testing.T, s *Store, from, to, content string) {
	t.Helper()
	if err := s.Append(from, Msg{Role: "user", Content: content}); err != nil {
		t.Fatal(err)
	}
	// 第一次：目标发布成功，来源墓碑写失败，记录停在 committing。
	deleted := filepath.Join(s.Dir(), "v1", "deleted")
	chmod(t, deleted, 0o500)
	if err := s.Rename(from, to); err == nil {
		t.Fatal("来源墓碑写入失败却报成功")
	}
	chmod(t, deleted, 0o700)
	// 第二次：目标已经有本操作的内容，跳过重写；墓碑与 committed 记录成功，
	// 最后清理源文件时目录不可写而失败。这是真实的 committed 现场。
	sessions := filepath.Join(s.Dir(), "v1", "sessions")
	chmod(t, sessions, 0o500)
	err := s.Rename(from, to)
	chmod(t, sessions, 0o700)
	if err == nil {
		t.Fatal("清理源文件失败却报成功")
	}
	b, rerr := os.ReadFile(s.renameOpFile(from, to))
	if rerr != nil {
		t.Fatalf("操作记录应该保留：%v", rerr)
	}
	var op renameOp
	if json.Unmarshal(b, &op) != nil || op.Stage != "committed" {
		t.Fatalf("现场不是 committed：%s", b)
	}
	if op.OperationID == "" || op.Version < 2 {
		t.Fatalf("真实记录缺少 operation_id / version：%s", b)
	}
}

// setupRenameBlockedAtTombstone 产生“目标已发布、记录停在 committing、来源墓碑写失败”的现场。
func setupRenameBlockedAtTombstone(t *testing.T, s *Store, from, to, content string) {
	t.Helper()
	if err := s.Append(from, Msg{Role: "user", Content: content}); err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(s.Dir(), "v1", "deleted")
	chmod(t, deleted, 0o500)
	err := s.Rename(from, to)
	chmod(t, deleted, 0o700)
	if err == nil {
		t.Fatal("来源墓碑写入失败却报成功")
	}
	b, rerr := os.ReadFile(s.renameOpFile(from, to))
	if rerr != nil {
		t.Fatalf("操作记录应该保留：%v", rerr)
	}
	var op renameOp
	if json.Unmarshal(b, &op) != nil || op.Stage != "committing" {
		t.Fatalf("现场不是 committing：%s", b)
	}
	if op.OperationID == "" || op.ToGen != 0 || op.ToRev != 0 {
		t.Fatalf("目标世代 / 版本没有绑定：%s", b)
	}
	if got := s.History(to); len(got) != 1 || got[0].Content != content {
		t.Fatalf("目标应该已经发布：%+v", got)
	}
}

func settledRenameStage(t *testing.T, s *Store) string {
	t.Helper()
	dir := filepath.Join(s.Dir(), "v1", "renames", "settled")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读不到终态记录：%v", err)
	}
	if len(entries) == 0 {
		t.Fatal("没有留下操作终态")
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[len(entries)-1].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var op renameOp
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatalf("终态记录损坏：%s", b)
	}
	return op.Stage
}

// Z1：空文件 / 未知文本都不能被当成可授权的世代，旧操作不得删除新代。
func TestMalformedGenerationCannotAuthorizeCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		preGen int64
		bad    string
	}{
		{"empty", 0, ""},
		{"unknown", 1, "broken-generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, _ := New(dir)
			from, to := "tg:z1a-"+tc.name, "tg:z1b-"+tc.name
			if tc.preGen == 1 {
				if err := s.Append(from, Msg{Role: "user", Content: "上一代"}); err != nil {
					t.Fatal(err)
				}
				if err := s.Delete(from); err != nil {
					t.Fatal(err)
				}
			}
			setupCommittedCleanupFailure(t, s, from, to, "源正文")

			// 同 sid 成功开新代，旧 committed 记录仍在。
			if err := s.Delete(from); err != nil {
				t.Fatal(err)
			}
			if err := s.Append(from, Msg{Role: "user", Content: "新代正文"}); err != nil {
				t.Fatal(err)
			}
			wantGen := tc.preGen + 1
			if n, err := s.readGen(from); err != nil || n != wantGen {
				t.Fatalf("新代世代：%d %v", n, err)
			}
			if err := os.WriteFile(s.genFile(from), []byte(tc.bad), 0o600); err != nil {
				t.Fatal(err)
			}

			err := s.ResumeRenames()
			if err == nil {
				t.Fatal("世代文件无效却恢复成功")
			}
			if !containsAny(err.Error(), "世代") {
				t.Fatalf("应该是世代解析错误：%v", err)
			}
			s2, _ := New(dir)
			if got := s2.History(from); len(got) != 1 || got[0].Content != "新代正文" {
				t.Fatalf("新代被旧操作删了：%+v", got)
			}
			if _, err := os.Stat(s.renameOpFile(from, to)); err != nil {
				t.Fatalf("出错时必须保留操作现场：%v", err)
			}

			// 修复世代文件后可以安全收尾：旧操作属于上一代，不再清理新代。
			if err := os.WriteFile(s.genFile(from), []byte(fmt.Sprintf("%d", wantGen)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := s.ResumeRenames(); err != nil {
				t.Fatalf("修复后收尾失败：%v", err)
			}
			s3, _ := New(dir)
			if got := s3.History(from); len(got) != 1 || got[0].Content != "新代正文" {
				t.Fatalf("收尾时动了新代：%+v", got)
			}
			if _, err := os.Stat(s.renameOpFile(from, to)); err == nil {
				t.Fatal("终态记录没有结束")
			}
		})
	}
}

// Z1 补充：世代格式必须严格；合法旧键仍兼容，负值 / 越界 / 非数字都报错。
func TestGenerationParsingIsStrict(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:gen-format"
	if n, err := s.readGen(sid); err != nil || n != 0 {
		t.Fatalf("文件不存在应是第 0 代：%d %v", n, err)
	}
	for _, tc := range []struct {
		body  string
		want  int64
		valid bool
	}{
		{"0", 0, true},
		{"7", 7, true},
		{Key(sid), 1, true},
		{"", 0, false},
		{"  ", 0, false},
		{"broken", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"00", 0, false},
		{"1.5", 0, false},
		{fmt.Sprintf("%d", maxCounter+1), 0, false},
		{"9223372036854775808", 0, false},
	} {
		if err := os.WriteFile(s.genFile(sid), []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := s.readGen(sid)
		if tc.valid {
			if err != nil || got != tc.want {
				t.Fatalf("readGen(%q) = %d, %v；期望 %d", tc.body, got, err, tc.want)
			}
		} else if err == nil {
			t.Fatalf("readGen(%q) 接受了无效格式：%d", tc.body, got)
		}
	}
}

// Z2：目标已经被用户 Delete，恢复旧改名不能清墓碑、写回旧上下文。
func TestPendingRenameCannotReviveDeletedTarget(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:z2d1", "tg:z2d2"
	setupRenameBlockedAtTombstone(t, s, from, to, "源正文")

	if err := s.Delete(to); err != nil {
		t.Fatal(err)
	}
	if !s.isDead(to) || s.Exists(to) {
		t.Fatal("删除没有生效")
	}

	s2, _ := New(dir)
	if err := s2.ResumeRenames(); err != nil {
		t.Fatalf("恢复应该把旧操作结算为失效：%v", err)
	}
	if got := s2.History(to); len(got) != 0 {
		t.Fatalf("旧上下文被恢复写回：%+v", got)
	}
	if !s2.isDead(to) {
		t.Fatal("恢复清掉了用户的删除墓碑")
	}
	if got := s2.History(from); len(got) != 1 || got[0].Content != "源正文" {
		t.Fatalf("源被越权清理：%+v", got)
	}
	if stage := settledRenameStage(t, s2); stage != "superseded" {
		t.Fatalf("终态应为 superseded：%q", stage)
	}
	if _, err := os.Stat(s2.renameOpFile(from, to)); err == nil {
		t.Fatal("活动记录没有结束")
	}
}

// Z2：目标已经被用户 Reset，恢复旧改名不能把 staging 原文填回去。
func TestTargetResetCannotBeUndoneByOldRename(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:z2r1", "tg:z2r2"
	setupRenameBlockedAtTombstone(t, s, from, to, "源正文")

	if err := s.Reset(to); err != nil {
		t.Fatal(err)
	}
	if got := s.History(to); len(got) != 0 {
		t.Fatalf("重置没有清空目标：%+v", got)
	}
	if rev, err := s.readRev(to); err != nil || rev != 1 {
		t.Fatalf("重置应推进上下文版本：%d %v", rev, err)
	}

	s2, _ := New(dir)
	if err := s2.ResumeRenames(); err != nil {
		t.Fatalf("恢复应该把旧操作结算为失效：%v", err)
	}
	if got := s2.History(to); len(got) != 0 {
		t.Fatalf("重置后的目标又被旧改名写回：%+v", got)
	}
	if s2.isDead(to) {
		t.Fatal("重置不是删除，不该留下墓碑")
	}
	if got := s2.History(from); len(got) != 1 || got[0].Content != "源正文" {
		t.Fatalf("源被越权清理：%+v", got)
	}
	if stage := settledRenameStage(t, s2); stage != "superseded" {
		t.Fatalf("终态应为 superseded：%q", stage)
	}
}

// 来源墓碑必须带 operation_id：用户后来删除源，旧改名不能把目标当成本操作已提交。
func TestUserDeletedSourceCannotCompletePendingRename(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:z2s1", "tg:z2s2"
	if err := s.Append(from, Msg{Role: "user", Content: "源正文"}); err != nil {
		t.Fatal(err)
	}
	// 让目标激活失败，停在 committing 之前的复制阶段。
	active := filepath.Join(dir, "v1", "active")
	chmod(t, active, 0o500)
	err := s.Rename(from, to)
	chmod(t, active, 0o700)
	if err == nil {
		t.Fatal("激活失败却报成功")
	}
	if err := s.Delete(from); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if err := s2.ResumeRenames(); err != nil {
		t.Fatalf("恢复失败：%v", err)
	}
	if got := s2.History(to); len(got) != 0 {
		t.Fatalf("用户删除源后，旧改名仍发布了目标：%+v", got)
	}
	if s2.Exists(to) {
		t.Fatal("目标被旧操作激活了")
	}
	if !s2.isDead(from) {
		t.Fatal("源的删除墓碑被恢复清掉")
	}
	if stage := settledRenameStage(t, s2); stage != "superseded" {
		t.Fatalf("终态应为 superseded：%q", stage)
	}
}

// Z2：目标已删除并重建为新代，旧改名不能把旧内容覆盖到新代上。
func TestTargetNewGenerationCannotBeOverwrittenByOldRename(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:z2n1", "tg:z2n2"
	setupRenameBlockedAtTombstone(t, s, from, to, "源正文")

	if err := s.Delete(to); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(to, Msg{Role: "user", Content: "目标新代"}); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if err := s2.ResumeRenames(); err != nil {
		t.Fatalf("恢复应该结算失效操作：%v", err)
	}
	got := s2.History(to)
	if len(got) != 1 || got[0].Content != "目标新代" {
		t.Fatalf("目标新代被旧改名污染：%+v", got)
	}
	if got := s2.History(from); len(got) != 1 || got[0].Content != "源正文" {
		t.Fatalf("源被越权清理：%+v", got)
	}
}

// 流式复制回归：大文件改名不能整份读入内存（否则低内存恢复会自己失败）。
func TestRenameLargeHistoryUsesStreamingCopy(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	from, to := "tg:big1", "tg:big2"
	if err := s.Append(from, Msg{Role: "user", Content: "先占位"}); err != nil {
		t.Fatal(err)
	}
	src := s.newPath(from, ".jsonl")
	line := []byte(`{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}` + "\n")
	f, err := os.OpenFile(src, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const copies = 16
	for i := 0; i < copies; i++ {
		if _, err := f.Write(line); err != nil {
			f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	wantLen := int64(len(line)) * copies
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := s.Rename(from, to); err != nil {
		t.Fatalf("大文件改名失败：%v", err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(wantLen/2) {
		t.Fatalf("改名分配了 %d 字节（文件 %d 字节），没有走有界流式复制", allocated, wantLen)
	}
	gotLen, _, err := hashFile(s.newPath(to, ".jsonl"))
	if err != nil || gotLen != wantLen {
		t.Fatalf("目标内容不完整：%d %v", gotLen, err)
	}
}

// 世代身份不可读或已经换代时，旧布局备份不能通过自动升级重新变成活动数据。
func TestUnauthorizedLegacyBackupIsNotPromoted(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  string
	}{
		{"corrupt", "broken-generation"},
		{"new-generation", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sid := "tg:legacy-" + tc.name
			legacy := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
			if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
				t.Fatal(err)
			}
			body := `{"role":"user","content":"旧备份"}` + "\n"
			if err := os.WriteFile(legacy, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "v1", "gen"), 0o700); err != nil {
				t.Fatal(err)
			}
			s, _ := New(dir)
			if err := os.WriteFile(s.genFile(sid), []byte(tc.gen), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := s.History(sid); len(got) != 0 {
				t.Fatalf("未被授权的旧备份被读出来了：%+v", got)
			}
			if _, err := os.Stat(legacy); err != nil {
				t.Fatalf("旧备份文件应保留：%v", err)
			}
			if _, err := os.Stat(s.activeMarker(sid)); err == nil {
				t.Fatal("旧备份被自动升级并写了激活标记")
			}
		})
	}
}
