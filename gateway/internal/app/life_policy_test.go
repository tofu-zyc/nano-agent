package app

import (
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
)

func ip(v int) *int       { return &v }
func sp(v string) *string { return &v }

// 档位默认值：low / normal / high 仍然是 4 / 8 / 16 次，间隔也跟随档位。
func TestEffectiveLifePolicyDefaultsFollowLevel(t *testing.T) {
	cases := []struct {
		level  string
		wakes  int
		reaches int
		min    time.Duration
	}{
		{"low", 4, 1, 2 * time.Hour},
		{"normal", 8, 2, time.Hour},
		{"high", 16, 4, 30 * time.Minute},
	}
	for _, c := range cases {
		p := effectiveLifePolicy(agents.Agent{LifeLevel: sp(c.level)})
		if p.MaxWakes != c.wakes || p.MaxReaches != c.reaches || p.Min != c.min {
			t.Fatalf("%s: %+v", c.level, p)
		}
		if !p.WakesFromLevel || !p.ReachesFromLevel || !p.WakeFromLevel {
			t.Fatalf("%s 应该报告“跟随档位”：%+v", c.level, p)
		}
	}
}

// 0 表示不限：不能出现 8/0 那种写法，也不能当成“没有额度”。
func TestEffectiveLifePolicyZeroMeansUnlimited(t *testing.T) {
	ag := agents.Agent{LifeLevel: sp("normal"), LifeMaxWakes: ip(0), LifeMaxReaches: ip(0)}
	p := effectiveLifePolicy(ag)
	if !p.WakesUnlimited() || !p.ReachesUnlimited() {
		t.Fatalf("0 应该是不限：%+v", p)
	}
	if got := p.wakeText(23); got != "今天探索 23 次 / 不限" {
		t.Fatalf("文字：%q", got)
	}
	if got := p.reachText(3); got != "今天主动联系 3 次 / 不限" {
		t.Fatalf("文字：%q", got)
	}
	if p.WakesFromLevel || p.ReachesFromLevel {
		t.Fatalf("显式设置了就不该说跟随档位：%+v", p)
	}
	// 次数不限不影响间隔：仍然有最短间隔，不会零间隔自旋
	if p.Min <= 0 || p.Max < p.Min {
		t.Fatalf("间隔不合法：%+v", p)
	}
}

// 正整数是上限；界面文字要写清楚“几次 / 上限”。
func TestEffectiveLifePolicyPositiveLimit(t *testing.T) {
	ag := agents.Agent{LifeMaxWakes: ip(3), LifeMaxReaches: ip(1)}
	p := effectiveLifePolicy(ag)
	if p.MaxWakes != 3 || p.MaxReaches != 1 {
		t.Fatalf("%+v", p)
	}
	if p.WakesUnlimited() || p.ReachesUnlimited() {
		t.Fatalf("3 和 1 不是不限：%+v", p)
	}
	if got := p.wakeText(2); got != "今天探索 2 次 / 3 次" {
		t.Fatalf("文字：%q", got)
	}
}

// 间隔独立于次数：只改次数时，间隔还是档位的。
func TestEffectiveLifePolicyIntervalOverrides(t *testing.T) {
	ag := agents.Agent{LifeLevel: sp("normal"), LifeWakeMin: sp("20m"), LifeWakeMax: sp("40m")}
	p := effectiveLifePolicy(ag)
	if p.Min != 20*time.Minute || p.Max != 40*time.Minute {
		t.Fatalf("%+v", p)
	}
	if p.MaxWakes != 8 {
		t.Fatalf("次数不该跟着间隔变：%+v", p)
	}
	// 只写最短：最长跟着最短走，不会出现 max < min
	p = effectiveLifePolicy(agents.Agent{LifeLevel: sp("normal"), LifeWakeMin: sp("3h")})
	if p.Min != 3*time.Hour || p.Max < p.Min {
		t.Fatalf("只写最短：%+v", p)
	}
	// 写反了：按小的在前处理，不产生负数区间
	p = effectiveLifePolicy(agents.Agent{LifeWakeMin: sp("2h"), LifeWakeMax: sp("30m")})
	if p.Min > p.Max {
		t.Fatalf("写反了没纠正：%+v", p)
	}
	// 写法不对：回落到档位
	p = effectiveLifePolicy(agents.Agent{LifeLevel: sp("low"), LifeWakeMin: sp("banana")})
	if p.Min != 2*time.Hour {
		t.Fatalf("非法写法应该回落到档位：%+v", p)
	}
}

// 保存时的校验：负数、非法时长、min > max 都要拒绝。
func TestValidateLifePolicy(t *testing.T) {
	bad := []agents.Agent{
		{LifeMaxWakes: ip(-1)},
		{LifeMaxReaches: ip(-2)},
		{LifeWakeMin: sp("abc")},
		{LifeWakeMax: sp("-1h")},
		{LifeWakeMin: sp("2h"), LifeWakeMax: sp("30m")},
	}
	for i, ag := range bad {
		if err := validateLifePolicy(ag); err == nil {
			t.Fatalf("第 %d 个应该被拒绝：%+v", i, ag)
		}
	}
	good := []agents.Agent{
		{},
		{LifeMaxWakes: ip(0), LifeMaxReaches: ip(0)},
		{LifeMaxWakes: ip(5)},
		{LifeWakeMin: sp("1h"), LifeWakeMax: sp("2h")},
		{LifeWakeMin: sp(""), LifeWakeMax: sp("")},
	}
	for i, ag := range good {
		if err := validateLifePolicy(ag); err != nil {
			t.Fatalf("第 %d 个不该被拒绝：%v", i, err)
		}
	}
}

// 醒来上限管的是“自选探索”：不限之后不会被“今天醒够了”挡住，到上限才挡。
func TestLifeBlockedHonoursUnlimitedWakes(t *testing.T) {
	e := setup(t)
	ag := agents.Agent{ID: "default", Style: "life", LifeReach: sp("off"), LifeMaxWakes: ip(0)}
	ls := e.a.lifeOf(ag.ID)
	ls.Wakes = 16 // 档位是 8，已经超了
	if why := e.a.lifeBlocked(ag, ls, e.a.now()); strings.Contains(why, "醒够") {
		t.Fatalf("不限次数还被挡住：%q", why)
	}
	ag.LifeMaxWakes = ip(16)
	if why := e.a.lifeBlocked(ag, ls, e.a.now()); !strings.Contains(why, "醒够") {
		t.Fatalf("到上限应该挡住：%q（而是 %q）", "醒够了", why)
	}
	// 上限减小时不杀已经启动的任务，只影响下一次调度
	ag.LifeMaxWakes = ip(3)
	ls.Wakes = 3
	if why := e.a.lifeBlocked(ag, ls, e.a.now()); !strings.Contains(why, "醒够") {
		t.Fatalf("上限 3、已经 3 次应该挡住：%q", why)
	}
}

// 主动联系次数：0 表示不限，但 LifeReach=off 仍然彻底关闭。
func TestReachLimitAndOff(t *testing.T) {
	p := effectiveLifePolicy(agents.Agent{LifeMaxReaches: ip(0)})
	if !p.ReachesUnlimited() {
		t.Fatalf("%+v", p)
	}
	p = effectiveLifePolicy(agents.Agent{LifeMaxReaches: ip(2)})
	if p.ReachesUnlimited() || p.MaxReaches != 2 {
		t.Fatalf("%+v", p)
	}
}
