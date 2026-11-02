package commit

import (
	"slices"
	"testing"
)

// 阶段表的每一行一个测试。
func TestPhaseTable(t *testing.T) {
	rows := []struct {
		name  string
		f     Facts
		phase string
		why   string
	}{
		{"1 done 没有证据", Facts{Ended: Done}, Blocked, "说做完了，但没有证据"},
		{"2 dropped 没有理由", Facts{Ended: Dropped}, Blocked, "说不用做了，但没写理由"},
		{"3 done", Facts{Ended: Done, Evidence: "测试通过"}, Done, "测试通过"},
		{"3 dropped", Facts{Ended: Dropped, Reason: "已经有人做了"}, Dropped, "已经有人做了"},
		{"3 failed", Facts{Ended: Failed, Reason: "接口一直超时"}, Failed, "接口一直超时"},
		{"3 stopped", Facts{Ended: Stopped, Reason: "主人取消了"}, Stopped, "主人取消了"},
		{"4 暂停", Facts{Paused: true, Running: true, Question: "q"}, Paused, ""},
		{"5 进行中", Facts{Running: true, Question: "q", Blocked: "b", Ready: "r"}, Running, ""},
		{"6 有问题", Facts{Question: "要不要重做？", Blocked: "b", Ready: "r"}, Blocked, "等你回答：要不要重做？"},
		{"7 受阻", Facts{Blocked: "上一次失败", Ready: "r"}, Blocked, "上一次失败"},
		{"8 就绪", Facts{Ready: "结果出来了", Due: 5, Now: 1}, Ready, "结果出来了"},
		{"9 等到某个时间", Facts{Now: 100, Due: 200, Wait: "w"}, Waiting, "等到某个时间"},
		{"10 到期了还不能开始", Facts{Now: 100, Due: 100, Wait: "等源会话的安全边界"}, Waiting, "等源会话的安全边界"},
		{"11 到期了马上开始", Facts{Now: 100, Due: 50}, Waiting, "到期了，马上开始"},
		{"12 等事件", Facts{Now: 100}, Waiting, "等事件"},
	}
	for _, r := range rows {
		if phase, why := Phase(r.f); phase != r.phase || why != r.why {
			t.Errorf("%s：得到 %s / %q，应为 %s / %q", r.name, phase, why, r.phase, r.why)
		}
	}
}

// 前面的行压住后面的行：每一行单独放一个更靠后的行的条件，前面那一行总是赢。
func TestPhaseOrder(t *testing.T) {
	later := Facts{Question: "q", Blocked: "b", Ready: "r", Now: 1, Due: 9, Wait: "w"}
	for _, c := range []struct {
		name  string
		extra func(*Facts)
		want  string
	}{
		{"结束压住暂停", func(f *Facts) { f.Ended, f.Evidence, f.Paused = Done, "e", true }, Done},
		{"暂停压住进行中", func(f *Facts) { f.Paused, f.Running = true, true }, Paused},
		{"进行中压住问题", func(f *Facts) { f.Running = true }, Running},
	} {
		f := later
		c.extra(&f)
		if got, _ := Phase(f); got != c.want {
			t.Errorf("%s：%s", c.name, got)
		}
	}
}

// 不变量：任意一组事实都恰好得到九个阶段中的一个；得到结束阶段时，一定带着证据或理由。
func TestPhaseInvariants(t *testing.T) {
	ends := []string{"", Done, Dropped, Failed, Stopped, "bogus"}
	texts := []string{"", "x"}
	bools := []bool{false, true}
	dues := []int64{0, 50, 100, 150}
	n := 0
	for _, ended := range ends {
		for _, ev := range texts {
			for _, reason := range texts {
				for _, paused := range bools {
					for _, running := range bools {
						for _, q := range texts {
							for _, b := range texts {
								for _, r := range texts {
									for _, w := range texts {
										for _, due := range dues {
											f := Facts{Now: 100, Ended: ended, Evidence: ev, Reason: reason, Paused: paused, Running: running,
												Question: q, Blocked: b, Ready: r, Wait: w, Due: due}
											phase, why := Phase(f)
											n++
											if !slices.Contains(Phases, phase) {
												t.Fatalf("%+v → %q 不是九个阶段之一", f, phase)
											}
											if IsEnded(phase) && why == "" {
												t.Fatalf("%+v → %s 没有带证据或理由", f, phase)
											}
											if IsEnded(phase) && ended != phase {
												t.Fatalf("%+v → %s，只有自己的记录里结束了才能是结束阶段", f, phase)
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if n < 6000 {
		t.Fatalf("组合太少：%d", n)
	}
}

func TestKindsAndPhasesAreFixed(t *testing.T) {
	if len(Kinds) != 6 || len(Phases) != 9 {
		t.Fatalf("%d 种 %d 个阶段", len(Kinds), len(Phases))
	}
	for _, p := range Phases[:5] {
		if IsEnded(p) {
			t.Errorf("%s 是进行中", p)
		}
	}
	for _, p := range Phases[5:] {
		if !IsEnded(p) {
			t.Errorf("%s 是结束", p)
		}
	}
}
