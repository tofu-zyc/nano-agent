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
