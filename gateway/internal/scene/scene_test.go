package scene

import (
	"encoding/json"
	"strings"
	"testing"
)

var tools = []string{"WebSearch", "Skill"}
var stageTools = []string{"WebSearch"}

func TestValidate(t *testing.T) {
	long := strings.Repeat("字", MaxText+1)
	bad := map[string]struct {
		r Rules
		k Kind
	}{
		"mode":       {Rules{Mode: "turns"}, Room},
		"dm free":    {Rules{Mode: ModeFree}, DMRoom},
		"brief":      {Rules{Brief: long}, Room},
		"role":       {Rules{Roles: map[string]Role{"a": {Prompt: long}}}, Room},
		"eager":      {Rules{Roles: map[string]Role{"a": {Eagerness: "x"}}}, Room},
		"tool":       {Rules{Tools: []string{"Shell"}}, Room},
		"negative":   {Rules{Budget: -1}, Room},
		"duration":   {Rules{Style: Style{Quiet: "abc"}}, Room},
		"guard low":  {Rules{Guard: Guard{Run: 1}}, Room},
		"guard hi":   {Rules{Guard: Guard{PingPong: 51}}, Room},
		"parts":      {Rules{Style: Style{Parts: 6}}, Room},
		"ext guard":  {Rules{Guard: Guard{Run: 5}}, External},
		"ext total":  {Rules{BudgetTotal: 5}, External},
		"stage dm":   {Rules{Stage: true}, DMRoom},
		"stage ext":  {Rules{Stage: true}, External},
		"inner":      {Rules{Inner: true}, Room},
		"stage tool": {Rules{Stage: true, Tools: []string{"Skill"}}, Room},
	}
	for name, c := range bad {
		if err := c.r.Validate(c.k, tools, stageTools); err == nil {
			t.Errorf("%s: 应该报错", name)
		}
	}
	good := Rules{Mode: ModeFree, Brief: "场景", Tools: []string{"Skill"}, Roles: map[string]Role{"a": {Prompt: "p", Eagerness: "high"}},
		Style: Style{Quiet: "30s"}, Guard: Guard{Run: 5, PingPong: 4}, Budget: 10, BudgetTotal: 100}
	if err := good.Validate(Room, tools, stageTools); err != nil {
		t.Fatal(err)
	}
	if err := (Rules{Stage: true, Inner: true, Tools: []string{"WebSearch"}}).Validate(Room, tools, stageTools); err != nil {
		t.Fatal(err)
	}
	if (Rules{Stage: true}).IsZero() {
		t.Fatal("开了 stage 就不是零值")
	}
	if err := (Rules{Mode: ModeMention, Tools: []string{}}).Validate(External, tools, stageTools); err != nil {
		t.Fatal(err)
	}
}

// tools 区分“缺省”和“空表”。
func TestToolsNullVersusEmpty(t *testing.T) {
	var a, b Rules
	json.Unmarshal([]byte(`{}`), &a)
	json.Unmarshal([]byte(`{"tools":[]}`), &b)
	if a.Tools != nil || b.Tools == nil || len(b.Tools) != 0 {
		t.Fatalf("缺省应为 nil，[] 应为空表：%v %v", a.Tools, b.Tools)
	}
	out, _ := json.Marshal(b)
	if !strings.Contains(string(out), `"tools":[]`) {
		t.Fatalf("空表要写回去：%s", out)
	}
}

func TestEffectiveGuardAndZero(t *testing.T) {
	run, pp := Guard{}.EffectiveGuard()
	if run != 8 || pp != 6 {
		t.Fatal(run, pp)
	}
	if !(Rules{}).IsZero() || (Rules{Tools: []string{}}).IsZero() {
		t.Fatal("IsZero")
	}
}

// 团队设置：只能在外部群；名单写法、工作目录、上限；设了团队的规则生效时固定点名、只记录。
func TestTeamValidateAndEffective(t *testing.T) {
	team := func(mod func(*Team)) Rules {
		t := &Team{Roster: []string{"telegram:123", "feishu:ou_x"}, Workspace: "ws1", Check: "make test"}
		if mod != nil {
			mod(t)
		}
		return Rules{Team: t}
	}
	if err := team(nil).Validate(External, nil, nil); err != nil {
		t.Fatalf("合法的团队：%v", err)
	}
	for _, kind := range []Kind{Room, DMRoom} {
		if err := team(nil).Validate(kind, nil, nil); err == nil {
			t.Errorf("%s 不能设团队", kind)
		}
	}
	for name, mod := range map[string]func(*Team){
		"没有工作目录":  func(x *Team) { x.Workspace = " " },
		"名单没有冒号":  func(x *Team) { x.Roster = []string{"123"} },
		"名单里有空格":  func(x *Team) { x.Roster = []string{"telegram: 123"} },
		"名单重复":    func(x *Team) { x.Roster = []string{"a:1", "a:1"} },
		"名单太长":    func(x *Team) { x.Roster = make([]string, MaxRoster+1) },
		"验收命令太长":  func(x *Team) { x.Check = strings.Repeat("x", MaxCheck+1) },
		"预算是负数":   func(x *Team) { x.Budget = -1 },
		"身份只有渠道名": func(x *Team) { x.Roster = []string{"telegram:"} },
	} {
		if err := team(mod).Validate(External, nil, nil); err == nil {
			t.Errorf("%s 应该被拒绝", name)
		}
	}
	r := Rules{Mode: ModeFree, Team: &Team{Workspace: "ws1"}}.Effective()
	if r.Mode != ModeMention || !r.Observe {
		t.Fatalf("设了团队：固定点名、只记录：%+v", r)
	}
	if r := (Rules{Mode: ModeFree}).Effective(); r.Mode != ModeFree || r.Observe {
		t.Fatalf("没设团队不变：%+v", r)
	}
	if (Rules{Team: &Team{}}).IsZero() {
		t.Fatal("设了团队不是零值")
	}
	// 克隆不共用名单
	a := team(nil)
	b := a.Clone()
	b.Team.Roster[0] = "x:1"
	if a.Team.Roster[0] != "telegram:123" {
		t.Fatal("Clone 共用了名单")
	}
	tm := Team{Roster: []string{"a:1"}}
	if !tm.InRoster("a:1") || tm.InRoster("a:2") || tm.InRoster("") || tm.EffectiveBudget() != DefaultTicketBudget || (Team{Budget: 5}).EffectiveBudget() != 5 {
		t.Fatal("名单、预算默认值")
	}
}
