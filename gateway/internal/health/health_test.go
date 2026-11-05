package health

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func fake() (*Board, *time.Time) {
	b := NewBoard()
	t := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return t }
	return b, &t
}

func TestClaimSetSnapshot(t *testing.T) {
	b, now := fake()
	h, err := b.Claim("storage")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Claim("storage"); err == nil {
		t.Fatal("同名第二次认领应报错")
	}
	if _, err := b.Claim(""); err == nil {
		t.Fatal("空名字应报错")
	}
	o, _ := b.Claim("outbox")
	got := b.Snapshot()
	if len(got) != 2 || got[0].Name != "outbox" || got[1].Name != "storage" || got[1].State != OK {
		t.Fatalf("按名字排序、初始 ok: %+v", got)
	}
	start := *now
	*now = now.Add(time.Minute)
	h.Set(Degraded, "三处")
	*now = now.Add(time.Minute)
	h.Set(Degraded, "四处") // 状态没变：Since 不变，说明更新
	e := b.Snapshot()[1]
	if e.State != Degraded || e.Detail != "四处" || !e.Since.Equal(start.Add(time.Minute)) {
		t.Fatalf("Since 是进入当前状态的时间: %+v", e)
	}
	*now = now.Add(time.Minute)
	h.Set(OK, "")
	if e := b.Snapshot()[1]; e.State != OK || e.Detail != "" || !e.Since.Equal(*now) {
		t.Fatalf("恢复: %+v", e)
	}
	o.Set("bogus", "x")
	if b.Snapshot()[0].State != OK {
		t.Fatal("未知状态应被忽略")
	}
}

func TestReleaseAndStaleHandle(t *testing.T) {
	b, _ := fake()
	old, _ := b.Claim("channel:tg")
	old.Set(Error, "掉线")
	old.Release()
	old.Release() // 可重复
	if len(b.Snapshot()) != 0 {
		t.Fatal("释放后条目应消失")
	}
	fresh, err := b.Claim("channel:tg")
	if err != nil {
		t.Fatalf("释放后同名可重新认领: %v", err)
	}
	old.Set(Error, "迟到的回报")
	if e := b.Snapshot()[0]; e.State != OK || e.Detail != "" {
		t.Fatalf("旧 Handle 不能写新条目: %+v", e)
	}
	fresh.Set(Degraded, "新的")
	old.Release() // 旧 Handle 的释放也不能撤掉新条目
	if e := b.Snapshot(); len(e) != 1 || e[0].Detail != "新的" {
		t.Fatalf("旧 Handle 的 Release 不能撤掉新条目: %+v", e)
	}
}

func TestNilIsNoop(t *testing.T) {
	var b *Board
	h, err := b.Claim("x")
	if err != nil || h != nil {
		t.Fatalf("nil board: %v %v", h, err)
	}
	h.Set(Error, "x")
	h.Release()
	if b.Snapshot() != nil {
		t.Fatal("nil board 没有条目")
	}
}

func TestDetailIsBounded(t *testing.T) {
	b, _ := fake()
	h, _ := b.Claim("x")
	h.Set(Error, strings.Repeat("错", 500))
	d := b.Snapshot()[0].Detail
	if len(d) > maxDetail || !utf8.ValidString(d) || d == "" {
		t.Fatalf("说明要有上限且不截断字符: %d %v", len(d), utf8.ValidString(d))
	}
}

func TestProblemsAndHasError(t *testing.T) {
	b, _ := fake()
	a, _ := b.Claim("a")
	c, _ := b.Claim("c")
	b.Claim("ok")
	if len(Problems(b.Snapshot())) != 0 || HasError(b.Snapshot()) {
		t.Fatal("全 ok")
	}
	a.Set(Degraded, "")
	if p := Problems(b.Snapshot()); len(p) != 1 || HasError(b.Snapshot()) {
		t.Fatalf("degraded 不算 error: %+v", p)
	}
	c.Set(Error, "")
	if p := Problems(b.Snapshot()); len(p) != 2 || !HasError(b.Snapshot()) {
		t.Fatalf("error: %+v", p)
	}
}

func TestConcurrent(t *testing.T) {
	b := NewBoard()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h, err := b.Claim("c")
				if err == nil {
					h.Set(Error, "x")
					b.Snapshot()
					h.Release()
				}
			}
		}()
	}
	wg.Wait()
	if len(b.Snapshot()) != 0 {
		t.Fatal("全部释放后应为空")
	}
}
