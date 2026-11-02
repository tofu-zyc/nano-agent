package commit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "commitments.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return time.UnixMilli(1000) }
	t.Cleanup(func() { s.Close() })
	return s, path
}

func report(kind, id, status, text string) CommitRec {
	return CommitRec{Type: TypeReport, Kind: kind, ID: id, Agent: "default", Status: status, Text: text}
}

func TestStoreKeepsLatestReportAndReplays(t *testing.T) {
	s, path := open(t)
	if _, err := s.Append(report(KindAgenda, "ag-1", StatusProgress, "开始了")); err != nil {
		t.Fatal(err)
	}
	r2, err := s.Append(report(KindAgenda, "ag-1", StatusBlocked, "卡住了"))
	if err != nil {
		t.Fatal(err)
	}
	s.Append(report(KindBranch, "b1", StatusReady, "好了"))
	if r2.Seq != 2 || r2.TS != 1000 {
		t.Fatalf("序号和时间：%+v", r2)
	}
	if got, _ := s.Latest(KindAgenda, "ag-1"); got.Text != "卡住了" || got.Seq != 2 {
		t.Fatalf("只留最近一条：%+v", got)
	}
	if _, ok := s.Latest(KindAgenda, "none"); ok {
		t.Fatal("没有的不该有")
	}
	// 重启后最近一条汇报还在，序号接着数
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, ok := s2.Latest(KindAgenda, "ag-1"); !ok || got.Text != "卡住了" {
		t.Fatalf("重启后：%+v %v", got, ok)
	}
	if r, _ := s2.Append(report(KindAgenda, "ag-1", StatusReady, "做完了")); r.Seq != 4 {
		t.Fatalf("序号要接着数：%d", r.Seq)
	}
}

func TestStoreSeenAndAnswerMarks(t *testing.T) {
	s, path := open(t)
	s.Append(report(KindBranch, "b1", StatusBlocked, "要决定"))
	if s.Seen(KindBranch, "b1", "r:1") {
		t.Fatal("还没看过")
	}
	s.Append(CommitRec{Type: TypeSeen, Kind: KindBranch, ID: "b1", Mark: "r:1", By: "web:me"})
	s.Append(CommitRec{Type: TypeAnswer, Kind: KindBranch, ID: "b1", Mark: "r:5", By: "web:me", Answer: "用第二个"})
	if !s.Seen(KindBranch, "b1", "r:1") || !s.Seen(KindBranch, "b1", "r:5") || s.Seen(KindBranch, "b1", "r:9") || s.Seen(KindBranch, "b1", "") {
		t.Fatal("看过和回答过都算")
	}
	if a, ok := s.Answer(KindBranch, "b1", "r:5"); !ok || a.Answer != "用第二个" {
		t.Fatalf("回答：%+v", a)
	}
	// 只留最近 16 个
	for i := 0; i < 20; i++ {
		s.Append(CommitRec{Type: TypeSeen, Kind: KindBranch, ID: "b1", Mark: fmt.Sprintf("m:%d", i)})
	}
	if s.Seen(KindBranch, "b1", "r:1") || !s.Seen(KindBranch, "b1", "m:19") || !s.Seen(KindBranch, "b1", "m:4") || s.Seen(KindBranch, "b1", "m:3") {
		t.Fatal("只留最近 16 个 Mark")
	}
	s.Close()
	s2, _ := Open(path)
	defer s2.Close()
	if !s2.Seen(KindBranch, "b1", "m:19") || s2.Seen(KindBranch, "b1", "m:3") {
		t.Fatal("重放后一样")
	}
}

func TestStoreRejectsBadRecords(t *testing.T) {
	s, _ := open(t)
	if _, err := s.Append(CommitRec{Type: TypeReport, Kind: KindAgenda}); err == nil {
		t.Fatal("没有 ID")
	}
	if _, err := s.Append(CommitRec{Type: "bogus", Kind: KindAgenda, ID: "x"}); err == nil {
		t.Fatal("不认识的种类")
	}
	// 太大的一条写不进去，内存不变
	big := report(KindAgenda, "x", StatusReady, strings.Repeat("字", 4000))
	if _, err := s.Append(big); err == nil {
		t.Fatal("超过单条上限要拒绝")
	}
	if _, ok := s.Latest(KindAgenda, "x"); ok {
		t.Fatal("写不进去，内存里也不能有")
	}
	// 写坏了（文件关了）：返回错误，内存不变
	s.Close()
	if _, err := s.Append(report(KindAgenda, "y", StatusReady, "好")); err == nil {
		t.Fatal("写不进去要返回错误")
	}
	if _, ok := s.Latest(KindAgenda, "y"); ok {
		t.Fatal("写不进去，内存里也不能有")
	}
}

// 压缩：只留每件事最近一条汇报和最近的 Mark；删掉的承诺的记录丢掉；重放后状态一样。
func TestStoreCompactDropsDeadCommitments(t *testing.T) {
	s, path := open(t)
	for i := 0; i < 50; i++ {
		s.Append(report(KindSchedule, "t1", StatusProgress, fmt.Sprintf("第 %d 次", i)))
		s.Append(report(KindSchedule, "dead", StatusProgress, "已经删掉的任务"))
	}
	s.Append(CommitRec{Type: TypeSeen, Kind: KindSchedule, ID: "t1", Mark: "r:99"})
	s.Append(CommitRec{Type: TypeSeen, Kind: KindSchedule, ID: "dead", Mark: "r:1"})
	before := s.Size()
	s.Alive = func() func(kind, id string) bool { return func(kind, id string) bool { return id != "dead" } }
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	if s.Size() >= before/10 {
		t.Fatalf("压缩后应该小得多：%d → %d", before, s.Size())
	}
	if _, ok := s.Latest(KindSchedule, "dead"); ok || s.Seen(KindSchedule, "dead", "r:1") {
		t.Fatal("删掉的承诺的记录要丢掉")
	}
	if r, _ := s.Latest(KindSchedule, "t1"); r.Text != "第 49 次" || !s.Seen(KindSchedule, "t1", "r:99") {
		t.Fatalf("活着的要留：%+v", r)
	}
	s.Close()
	s2, _ := Open(path)
	defer s2.Close()
	if r, _ := s2.Latest(KindSchedule, "t1"); r.Text != "第 49 次" || !s2.Seen(KindSchedule, "t1", "r:99") {
		t.Fatalf("重放后：%+v", r)
	}
	// 序号接着留下来的最大的数：新的汇报不会和留着的 Mark 撞上
	if r, _ := s2.Latest(KindSchedule, "t1"); s2.seq < r.Seq {
		t.Fatalf("序号倒退到了留着的记录前面：%d < %d", s2.seq, r.Seq)
	}
}

// 超过 8MB 自动整份重写；坏掉的文件拒绝打开，文件保持原样。
func TestStoreAutoCompactAndCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	if err := os.WriteFile(path, []byte("这不是日志\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("坏文件要拒绝打开")
	}
	if b, _ := os.ReadFile(path); string(b) != "这不是日志\n" {
		t.Fatalf("文件不能动：%q", b)
	}
}

// progress、waiting 顶不掉没处理的汇报：每件事留两条，最近一条汇报和最近一条要主人知道的汇报。
func TestStoreProgressDoesNotReplaceNotice(t *testing.T) {
	s, path := open(t)
	s.Append(report(KindAgenda, "ag-1", StatusBlocked, "有两份重复的"))
	s.Append(CommitRec{Type: TypeReport, Kind: KindAgenda, ID: "ag-1", Status: StatusBlocked, Text: "要决定", Question: "留哪一份？"})
	s.Append(report(KindAgenda, "ag-1", StatusProgress, "又看了一遍"))
	s.Append(report(KindAgenda, "ag-1", StatusWaiting, "在等回答"))
	latest, _ := s.Latest(KindAgenda, "ag-1")
	notice, ok := s.Notice(KindAgenda, "ag-1")
	if latest.Status != StatusWaiting || !ok || notice.Question != "留哪一份？" || notice.Seq != 2 {
		t.Fatalf("最近一条 %+v，要主人知道的 %+v", latest, notice)
	}
	if _, ok := s.Notice(KindAgenda, "none"); ok {
		t.Fatal("没有的不该有")
	}
	// 只有 progress 的：没有要主人知道的
	s.Append(report(KindBranch, "b1", StatusProgress, "在跑"))
	if _, ok := s.Notice(KindBranch, "b1"); ok {
		t.Fatal("progress 不算要主人知道的")
	}
	// 压缩时两条都留，重放后状态一样
	for i := 0; i < 5; i++ {
		s.Append(report(KindAgenda, "ag-1", StatusProgress, "再来一条"))
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	check := func(s *Store, when string) {
		t.Helper()
		l, _ := s.Latest(KindAgenda, "ag-1")
		n, ok := s.Notice(KindAgenda, "ag-1")
		if l.Seq != 10 || !ok || n.Seq != 2 || n.Question != "留哪一份？" {
			t.Fatalf("%s：latest=%+v notice=%+v", when, l, n)
		}
	}
	check(s, "压缩后")
	s.Close()
	s2, _ := Open(path)
	defer s2.Close()
	check(s2, "重放后")
}
