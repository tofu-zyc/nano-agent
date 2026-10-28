package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupPendingPromotion 产生真实的“清单已 verified、目标已复制、active 写失败”现场。
// 返回的操作记录是产品实际写入的，不是手工拼的 JSON。
func setupPendingPromotion(t *testing.T, s *Store, sid, history string) promoteOp {
	t.Helper()
	writeFile(t, filepath.Join(s.Dir(), "sessions", LegacyKey(sid)+".jsonl"), history)
	active := filepath.Join(s.Dir(), "v1", "active")
	if err := os.RemoveAll(active); err != nil {
		t.Fatal(err)
	}
	writeFile(t, active, "x")
	if err := s.promote(sid); err == nil {
		t.Fatal("active 写失败却报成功")
	}
	op, ok, err := s.readPromoteOp(sid)
	if err != nil || !ok {
		t.Fatalf("提升记录应该保留：%v", err)
	}
	if op.Stage != "verified" {
		t.Fatalf("现场不是 verified：%+v", op)
	}
	for _, item := range op.Items {
		if item.Stage != "verified" {
			t.Fatalf("现场项目没有 verified：%+v", op.Items)
		}
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	return op
}

func settledPromoteOp(t *testing.T, s *Store) promoteOp {
	t.Helper()
	dir := filepath.Join(s.Dir(), "v1", "promotions", "settled")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读不到提升终态：%v", err)
	}
	if len(entries) == 0 {
		t.Fatal("没有提升终态记录")
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[len(entries)-1].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var op promoteOp
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatalf("提升终态损坏：%s", b)
	}
	return op
}

// 复制完成但状态没记下来的现场：记录仍是 planned，目标已经与清单一致。
// 恢复必须按 operation_id 读取原清单，核对目标内容后复用，而不是重新生成计划。
func TestPromotionRecoversUnrecordedCopy(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22a"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	op := setupPendingPromotion(t, s, sid, history)

	op.Stage = "copying"
	for i := range op.Items {
		op.Items[i].Stage = "planned"
	}
	if err := s.writePromoteOp(op); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "v1", "active"), 0o700); err != nil {
		t.Fatal(err)
	}

	s2, _ := New(dir)
	if err := s2.ResumePromotions(); err != nil {
		t.Fatalf("启动恢复失败：%v", err)
	}
	if got := s2.History(sid); len(got) != 1 || got[0].Content != "旧正文" {
		t.Fatalf("恢复后目标历史：%+v", got)
	}
	if _, err := os.Stat(s2.activeMarker(sid)); err != nil {
		t.Fatal("恢复后没有激活标记")
	}
	if _, err := os.Stat(s2.promoteOpPath(sid)); err == nil {
		t.Fatal("活动提升记录没有收尾")
	}
	settled := settledPromoteOp(t, s2)
	if settled.Stage != "committed" || settled.OperationID != op.OperationID {
		t.Fatalf("恢复用了另一份计划或没提交：%+v", settled)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")); err != nil {
		t.Fatal("旧源文件不应被删除")
	}
}

// 状态已 verified 后源被改写：必须报冲突，不能激活，也不能覆盖目标。
func TestPromotionVerifiedSourceChangeConflicts(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22s"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	src := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	setupPendingPromotion(t, s, sid, history)
	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, append(append([]byte(nil), original...), []byte(`{"role":"user","content":"后来写的"}`+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.promote(sid); err == nil {
		t.Fatal("源变化后仍激活成功")
	}
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		t.Fatal("冲突时写了激活标记")
	}
	op, ok, err := s.readPromoteOp(sid)
	if err != nil || !ok || op.Stage != "conflict" {
		t.Fatalf("冲突没有登记：%+v %v", op, err)
	}
	if op.Items[0].Stage != "conflict" {
		t.Fatalf("冲突项阶段：%+v", op.Items[0])
	}
	target, err := os.ReadFile(s.newPath(sid, ".jsonl"))
	if err != nil || string(target) != history {
		t.Fatalf("目标被冲突操作改了：%q %v", target, err)
	}
	// 源恢复原样后可以继续，不需要丢掉清单重新开始。
	if err := os.WriteFile(src, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.promote(sid); err != nil {
		t.Fatalf("修复源之后不能续办：%v", err)
	}
	if _, err := os.Stat(s.activeMarker(sid)); err != nil {
		t.Fatal("续办后没有激活标记")
	}
}

// 状态已 verified 后目标被改写：保留目标现场，报冲突，不激活。
func TestPromotionVerifiedTargetChangeConflicts(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22t"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	setupPendingPromotion(t, s, sid, history)
	target := s.newPath(sid, ".jsonl")
	changed := history + `{"role":"user","content":"目标后来写的"}` + "\n"
	if err := os.WriteFile(target, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.promote(sid); err == nil {
		t.Fatal("目标变化后仍激活成功")
	}
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		t.Fatal("冲突时写了激活标记")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != changed {
		t.Fatalf("目标现场被覆盖：%q %v", got, err)
	}
	op, ok, err := s.readPromoteOp(sid)
	if err != nil || !ok || op.Stage != "conflict" {
		t.Fatalf("冲突没有登记：%+v %v", op, err)
	}
}

// 提升清单写入失败不能激活；恢复后可以按原计划继续。
func TestPromotionManifestWriteFailureCannotActivate(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22w"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	op := setupPendingPromotion(t, s, sid, history)
	promotions := filepath.Join(dir, "v1", "promotions")
	chmod(t, promotions, 0o500)
	if err := s.promote(sid); err == nil {
		t.Fatal("清单写失败却报成功")
	}
	chmod(t, promotions, 0o700)
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		t.Fatal("清单写失败却激活了")
	}
	cur, ok, err := s.readPromoteOp(sid)
	if err != nil || !ok || cur.OperationID != op.OperationID || cur.Stage != "verified" {
		t.Fatalf("失败时必须保留原清单：%+v %v", cur, err)
	}
	if err := s.promote(sid); err != nil {
		t.Fatalf("修好清单写入后不能续办：%v", err)
	}
	if _, err := os.Stat(s.activeMarker(sid)); err != nil {
		t.Fatal("续办后没有激活标记")
	}
}

// 后续 Delete / 同 sid 换代必须让旧提升失效，不能把旧备份重新激活。
func TestPromotionAfterDeleteDoesNotReactivate(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22d"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	setupPendingPromotion(t, s, sid, history)
	if err := s.Delete(sid); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	if err := s2.ResumePromotions(); err != nil {
		t.Fatalf("删除后的提升记录应该安全结算：%v", err)
	}
	if _, err := os.Stat(s2.activeMarker(sid)); err == nil {
		t.Fatal("删除后又被旧提升激活")
	}
	if got := s2.History(sid); len(got) != 0 {
		t.Fatalf("删除后又复活：%+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")); err != nil {
		t.Fatal("旧备份应保留")
	}
	if op := settledPromoteOp(t, s2); op.Stage != "superseded" {
		t.Fatalf("终态应为 superseded：%+v", op)
	}

	if err := s2.Append(sid, Msg{Role: "user", Content: "新代"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.ResumePromotions(); err != nil {
		t.Fatalf("新代启动恢复不应报错：%v", err)
	}
	got := s2.History(sid)
	if len(got) != 1 || got[0].Content != "新代" {
		t.Fatalf("旧提升污染了新代：%+v", got)
	}
}

// 上下文 revision 在提升期间被推进（Reset 等后续失效）：旧提升不得激活。
func TestPromotionAfterRevisionChangeDoesNotReactivate(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22r"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	setupPendingPromotion(t, s, sid, history)
	if _, err := s.bumpRev(sid); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	err := s2.ResumePromotions()
	if err == nil || !containsAny(err.Error(), "提升操作已因后续失效被取消") {
		t.Fatalf("revision 变化后应该拒绝续办：%v", err)
	}
	if _, err := os.Stat(s2.activeMarker(sid)); err == nil {
		t.Fatal("revision 变化后仍被激活")
	}
	if op := settledPromoteOp(t, s2); op.Stage != "superseded" {
		t.Fatalf("终态应为 superseded：%+v", op)
	}
}

// 归档的每个文件都要进入清单和报告，不能只汇总历史 / 设置 / 分支。
func TestMigrateReportsEveryArchiveItem(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22arch"
	writeFile(t, filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl"),
		`{"role":"user","content":"正文"}`+"\n")
	writeFile(t, filepath.Join(dir, "archive", legacyArchiveKey(sid), "a.jsonl"),
		`{"role":"user","content":"归档A"}`+"\n")
	writeFile(t, filepath.Join(dir, "archive", legacyArchiveKey(sid), "b.jsonl"),
		`{"role":"user","content":"归档B"}`+"\n")
	rec, err := s.Migrate()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]MigrationItem{}
	for _, item := range rec.Items {
		seen[item.Kind+":"+item.Name] = item
	}
	for _, key := range []string{"history:", "archive:a.jsonl", "archive:b.jsonl"} {
		item, ok := seen[key]
		if !ok {
			t.Fatalf("报告缺少 %s：%+v", key, rec.Items)
		}
		if item.Stage != "verified" || item.Bytes == 0 || !validSum(item.Sum) || item.OperationID == "" {
			t.Fatalf("归档项不可恢复字段不全：%+v", item)
		}
	}
	if _, err := os.Stat(s.activeMarker(sid)); err != nil {
		t.Fatal("迁移完成后没有激活标记")
	}
	b, err := os.ReadFile(filepath.Join(dir, "v1", "sessions", "keys", "migration.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"operation_id", "a.jsonl", "b.jsonl"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("migration.json 缺少 %q：%s", want, b)
		}
	}
}

// 真实大文件提升也必须走有界流式复制，不能按整份历史分配。
func TestPromotionLargeHistoryUsesStreamingCopy(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22big"
	line := []byte(`{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}` + "\n")
	src := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
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
	if err := s.promote(sid); err != nil {
		t.Fatalf("大文件提升失败：%v", err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("promotion TotalAlloc delta = %d bytes for %d-byte history", allocated, wantLen)
	if allocated > uint64(wantLen/2) {
		t.Fatalf("提升分配了 %d 字节（文件 %d 字节），没有走有界流式复制", allocated, wantLen)
	}
	gotLen, gotSum, err := hashFile(s.newPath(sid, ".jsonl"))
	if err != nil || gotLen != wantLen {
		t.Fatalf("目标内容不完整：%d %v", gotLen, err)
	}
	_, wantSum, err := hashFile(src)
	if err != nil || gotSum != wantSum {
		t.Fatalf("目标摘要不一致：%s != %s (%v)", gotSum, wantSum, err)
	}
}

// locate 触发的提升也必须读取并续办原清单，而不是看到目标存在就宣告完成。
func TestPromotionRecoversThroughLocate(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22loc"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	op := setupPendingPromotion(t, s, sid, history)
	op.Stage = "copying"
	for i := range op.Items {
		op.Items[i].Stage = "copying"
	}
	if err := s.writePromoteOp(op); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(dir)
	got := s2.History(sid)
	if len(got) != 1 || got[0].Content != "旧正文" {
		t.Fatalf("locate 恢复后的历史：%+v", got)
	}
	if _, err := os.Stat(s2.activeMarker(sid)); err != nil {
		t.Fatal("locate 恢复后没有激活标记")
	}
	settled := settledPromoteOp(t, s2)
	if settled.OperationID != op.OperationID || settled.Stage != "committed" {
		t.Fatalf("locate 没有续办原清单：%+v", settled)
	}
}

// 真实的 Reset 也必须让待完成提升失效；最终上下文为空，不能把旧正文带回来。
func TestPromotionAfterRealResetDoesNotRestoreContext(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22rst"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	setupPendingPromotion(t, s, sid, history)
	if err := s.Reset(sid); err != nil {
		t.Fatalf("Reset 失败：%v", err)
	}
	if got := s.History(sid); len(got) != 0 {
		t.Fatalf("Reset 后旧正文复活：%+v", got)
	}
	if _, err := os.Stat(s.activeMarker(sid)); err != nil {
		t.Fatal("Reset 后会话应该仍然存在")
	}
	s2, _ := New(dir)
	if err := s2.ResumePromotions(); err != nil {
		t.Fatalf("Reset 后启动恢复不应报错：%v", err)
	}
	if got := s2.History(sid); len(got) != 0 {
		t.Fatalf("启动恢复又把旧正文带回来：%+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")); err != nil {
		t.Fatal("旧备份应保留")
	}
}

// migration.json 只是运行报告；它写失败不能让已提交的提升变得不可恢复，
// 也不能反过来重新激活 / 覆盖数据。
func TestMigrateSummaryFailureKeepsPromotionRecoverable(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22sum"
	writeFile(t, filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl"),
		`{"role":"user","content":"旧正文"}`+"\n")
	keys := filepath.Join(dir, "v1", "sessions", "keys")
	chmod(t, keys, 0o500)
	if _, err := s.Migrate(); err == nil {
		t.Fatal("汇总清单写失败却报成功")
	}
	chmod(t, keys, 0o700)
	if _, err := os.Stat(s.activeMarker(sid)); err != nil {
		t.Fatal("逐项清单已提交的提升不应因汇总失败而丢失")
	}
	if got := s.History(sid); len(got) != 1 || got[0].Content != "旧正文" {
		t.Fatalf("提升后的历史：%+v", got)
	}
	if op := settledPromoteOp(t, s); op.Stage != "committed" {
		t.Fatalf("逐项提升终态：%+v", op)
	}
	if _, err := s.Migrate(); err != nil {
		t.Fatalf("修复汇总目录后重跑失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(keys, "migration.json")); err != nil {
		t.Fatalf("修复后应该能写汇总：%v", err)
	}
}

// 第一份计划都没登记成功时不能开始复制。
func TestPromotionInitialManifestFailureDoesNotCopy(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "tg:p22init"
	history := `{"role":"user","content":"旧正文"}` + "\n"
	src := filepath.Join(dir, "sessions", LegacyKey(sid)+".jsonl")
	writeFile(t, src, history)
	promotions := filepath.Join(dir, "v1", "promotions")
	chmod(t, promotions, 0o500)
	if err := s.promote(sid); err == nil {
		t.Fatal("初始清单写失败却报成功")
	}
	chmod(t, promotions, 0o700)
	if _, err := os.Stat(s.newPath(sid, ".jsonl")); err == nil {
		t.Fatal("初始清单没登记就开始复制了")
	}
	if _, err := os.Stat(s.activeMarker(sid)); err == nil {
		t.Fatal("初始清单没登记就激活了")
	}
	if got, err := os.ReadFile(src); err != nil || string(got) != history {
		t.Fatalf("旧文件应保留：%q %v", got, err)
	}
	if err := s.promote(sid); err != nil {
		t.Fatalf("修复后不能提升：%v", err)
	}
}
