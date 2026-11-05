package autonomy

import (
	"testing"
	"time"
)

// 今天用了多少 token 只看账本：各个来源和“其他”加起来；BudgetLeft 从它算。
func TestBudgetComesFromLedger(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, testLoc)
	s := New()
	s.RollDay(now, testLoc)
	s.ReserveLedger(LedgerExploration, "r1", now, testLoc)
	s.SettleLedger(LedgerExploration, "r1", "run-1", 100, now, testLoc)
	s.ReserveLedger(LedgerManual, "r2", now, testLoc)
	s.SettleLedger(LedgerManual, "r2", "run-2", 30, now, testLoc)
	s.AddLedgerTokens(LedgerExploration, 20, now, testLoc) // 被打断的活动：只记 token，次数不动
	s.SpendAt(50, now, testLoc)                            // 自主分支
	if got := s.TokensOn("2026-10-21"); got != 200 {
		t.Fatalf("今天的总数：%d", got)
	}
	if s.BudgetLeft(1000) != 800 || s.Budget.Tokens != 200 {
		t.Fatalf("预算：剩 %d，副本 %d", s.BudgetLeft(1000), s.Budget.Tokens)
	}
	if starts := s.LedgerStarts(LedgerExploration, now, testLoc); starts != 1 {
		t.Fatalf("只记 token 的不加次数：%d", starts)
	}
	// 换天：新的一天从 0 开始，昨天的账还在
	next := now.Add(24 * time.Hour)
	if !s.RollDay(next, testLoc) || s.BudgetLeft(1000) != 1000 || s.Budget.Tokens != 0 {
		t.Fatalf("换天：剩 %d 副本 %d", s.BudgetLeft(1000), s.Budget.Tokens)
	}
	if s.TokensOn("2026-10-21") != 200 {
		t.Fatal("昨天的账不能丢")
	}
}

// 找人记进账本的 reach 来源，只记次数；网页上的四个来源不变。
func TestReachInLedger(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, testLoc)
	s := New()
	s.RecordReach(s.NewID("reach"), now, testLoc)
	s.RecordReach(s.NewID("reach"), now, testLoc)
	if got := s.LedgerStarts(LedgerReach, now, testLoc); got != 2 {
		t.Fatalf("找了两次：%d", got)
	}
	rows := s.LedgerToday(now, testLoc)
	if len(rows) != 4 {
		t.Fatalf("显示的来源仍是四个：%d", len(rows))
	}
	for _, r := range rows {
		if r.Source == LedgerReach || r.Source == LedgerOther {
			t.Fatalf("reach 和 other 不在显示里：%+v", r)
		}
	}
	// 导入旧计数：只升不降
	s.RaiseLedgerStarts(LedgerReach, 1, now, testLoc)
	if s.LedgerStarts(LedgerReach, now, testLoc) != 2 {
		t.Fatal("不能往下降")
	}
	s.RaiseLedgerStarts(LedgerReach, 5, now, testLoc)
	if s.LedgerStarts(LedgerReach, now, testLoc) != 5 {
		t.Fatal("要升到 5")
	}
}

// 版本 3：旧版本（2）的状态加载后不自动变成 3，要由导入标记；未来版本不动。
func TestSchema3IsSetByLifeImport(t *testing.T) {
	s := &State{SchemaVersion: 2}
	s.Normalize()
	if s.SchemaVersion != 2 {
		t.Fatalf("Normalize 不替导入改版本：%d", s.SchemaVersion)
	}
	s.MarkLifeImported()
	if s.SchemaVersion != 3 || s.Migrated&MigratedLifeJSON == 0 {
		t.Fatalf("%+v", s)
	}
	if New().SchemaVersion != 3 {
		t.Fatal("新状态直接是版本 3")
	}
}
