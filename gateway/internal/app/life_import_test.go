package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/health"
	"nano-agent/gateway/internal/knowledge"
)

// 旧的 data/life.json 并进 data/autonomy.json（设计 §15.1 R2）。

var importDay = time.Date(2026, 10, 21, 15, 8, 0, 0, time.UTC)

const importLifeJSON = `{
  "default": {
    "experience_imported": false,
    "next": 1792622400,
    "day": "2026-10-21",
    "wakes": 3,
    "reaches": 1,
    "tokens": 450,
    "recent": [{"at": 1792580000, "text": "看了一篇讲猫的文章"}, {"at": 1792583000, "text": "整理了书架"}],
    "awaiting": {"main": 1792590000},
    "follow_ups": {"tg:1": {"at": 1792600000, "note": "问问面试怎么样"}}
  }
}`

// 升级前的 autonomy.json：版本 2，账本里探索 1 次（旧的 life.json 记了 3 次），预算里记了 450 token（含自主分支）
const importAutonomyJSON = `{
  "version": 2,
  "agents": {
    "default": {
      "schema_version": 2,
      "budget": {"day": "2026-10-21", "tokens": 450, "limit": 1000},
      "ledger": [{"day": "2026-10-21", "source": "exploration", "starts": 1, "tokens": 200}]
    }
  }
}`

// importApp 在一个已经放好旧文件的数据目录上启动网关，agent default 是拟人型。
func importApp(t *testing.T, dir string, life, autonomyDoc string) *App {
	t.Helper()
	data := filepath.Join(dir, "data")
	os.MkdirAll(data, 0o755)
	if life != "" {
		os.WriteFile(filepath.Join(data, "life.json"), []byte(life), 0o600)
	}
	if autonomyDoc != "" {
		os.WriteFile(filepath.Join(data, "autonomy.json"), []byte(autonomyDoc), 0o600)
	}
	a := newAppAt(t, dir, nil)
	a.now = func() time.Time { return importDay }
	a.loc = time.UTC
	store, err := agents.Open(filepath.Join(data, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	bots, err := agents.OpenBots(filepath.Join(data, "bots.json"))
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgents(store, bots, filepath.Join(dir, "agents"))
	if err := a.MigrateAgents(); err != nil {
		t.Fatal(err)
	}
	ag, _ := store.Get(agents.DefaultID)
	ag.Style = agents.StyleLife
	budget := 1000
	ag.LifeBudget = &budget
	if err := store.Update(ag); err != nil {
		t.Fatal(err)
	}
	return a
}

func autonomyHealth(a *App) health.Entry {
	for _, h := range a.health.Snapshot() {
		if h.Name == "autonomy" {
			return h
		}
	}
	return health.Entry{}
}

// 网页（lifeBrief）和状态（autonomy 视图）显示的数字。
type shown struct {
	Next, Wakes, Reaches, Tokens, BudgetTokens, BudgetLeft int64
	Waiting, FollowUps                                     int
	Recent                                                 string
}

func showNumbers(t *testing.T, a *App) shown {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	ag, _ := a.agentByID("default")
	b := a.lifeBrief(ag)
	v := a.mindOf("default").View(a.now(), a.loc, 6)
	return shown{Next: b.Next, Wakes: int64(b.Wakes), Reaches: int64(b.Reaches), Tokens: int64(b.Tokens),
		BudgetTokens: int64(v.Budget.Tokens), BudgetLeft: int64(v.BudgetLeft),
		Waiting: len(b.Waiting), FollowUps: len(a.mind("default").FollowUps), Recent: a.latestLifeNote("default")}
}

func TestImportLifeMergesIntoAutonomy(t *testing.T) {
	dir := t.TempDir()
	a := importApp(t, dir, importLifeJSON, importAutonomyJSON)
	got := showNumbers(t, a)
	// 导入前这些页面显示的数字：下次醒来 1792622400、醒了 3 次（旧 life.json 和账本里取大的）、找了 1 次、
	// 用了 450 token（预算里也是 450）、预算剩 550、在等 1 个人、1 个回访、近况是最后一条
	want := shown{Next: 1792622400 * 1000, Wakes: 3, Reaches: 1, Tokens: 450, BudgetTokens: 450, BudgetLeft: 550,
		Waiting: 1, FollowUps: 1, Recent: "整理了书架"}
	if got != want {
		t.Fatalf("导入后的数字：\n%+v\n应为\n%+v", got, want)
	}
	// life.json 改名了，没删
	if _, err := os.Stat(filepath.Join(dir, "data", "life.json")); !os.IsNotExist(err) {
		t.Fatalf("life.json 应该改名：%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "data", "life.json.imported")); err != nil || string(b) != importLifeJSON {
		t.Fatalf("life.json.imported：%v", err)
	}
	// autonomy.json 升到版本 3，记了导入；旧版本留了备份
	b, _ := os.ReadFile(filepath.Join(dir, "data", "autonomy.json"))
	var doc autonomyDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	st := doc.Agents["default"]
	if doc.Version != 3 || st.SchemaVersion != 3 || st.Migrated&autonomy.MigratedLifeJSON == 0 {
		t.Fatalf("版本和导入标记：%d %d %d", doc.Version, st.SchemaVersion, st.Migrated)
	}
	if st.NextCheck != 1792622400 || st.Awaiting["main"] != 1792590000 || st.FollowUps["tg:1"].Note != "问问面试怎么样" {
		t.Fatalf("导进来的状态：%+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "autonomy.json.schema2.bak")); err != nil {
		t.Fatalf("升级前的 autonomy.json 要留一份备份：%v", err)
	}
	// 旧近况进了经历，只进一次
	a.mu.Lock()
	n := 0
	for _, e := range a.experienceOf("default").Recent(knowledge.Viewer{Admin: true}, "", 50, a.now()) {
		if e.Kind == "life" && strings.HasPrefix(e.Sources[0], "legacy-recent:") {
			n++
		}
	}
	a.mu.Unlock()
	if n != 2 {
		t.Fatalf("旧近况导进经历 %d 条，应为 2", n)
	}
	if h := autonomyHealth(a); h.State != health.OK {
		t.Fatalf("健康条目：%+v", h)
	}

	// 再启动一次：不重复导入，数字不变（就算旧文件又出现了，已经导入过的 agent 不再导）
	os.WriteFile(filepath.Join(dir, "data", "life.json"), []byte(importLifeJSON), 0o600)
	a2 := newAppAt(t, dir, nil)
	a2.now = func() time.Time { return importDay }
	a2.loc = time.UTC
	a2.mu.Lock()
	st2 := a2.mind("default")
	wakes, reaches, tokens := a2.wakesToday(st2, importDay), a2.reachesToday(st2, importDay), a2.tokensToday(st2, importDay)
	a2.mu.Unlock()
	if wakes != 3 || reaches != 1 || tokens != 450 {
		t.Fatalf("再启动不该重复导入：%d %d %d", wakes, reaches, tokens)
	}
}

// 没有 life.json（新装的网关）：不导入也不报错，状态直接是版本 3。
func TestImportLifeNothingToImport(t *testing.T) {
	dir := t.TempDir()
	a := importApp(t, dir, "", importAutonomyJSON)
	a.mu.Lock()
	st := a.mind("default")
	a.mu.Unlock()
	if st.SchemaVersion != 3 || st.Migrated&autonomy.MigratedLifeJSON == 0 {
		t.Fatalf("%+v", st)
	}
	if got := a.tokensToday(st, importDay); got != 450 {
		t.Fatalf("预算里记的 450 token 要在账本里：%d", got)
	}
	if h := autonomyHealth(a); h.State != health.OK {
		t.Fatalf("健康条目：%+v", h)
	}
}

// 导入失败：两个文件都不动，健康条目 autonomy 变成 error，自主活动不启动。
func TestImportLifeFailureTouchesNothing(t *testing.T) {
	cases := map[string]func(dir string) string{
		"life.json 读不懂": func(dir string) string { return "{这不是 json" },
		"autonomy.json 写不进去": func(dir string) string {
			os.MkdirAll(filepath.Join(dir, "data", "autonomy.json.tmp"), 0o755) // 临时文件的位置被目录占了
			return importLifeJSON
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.MkdirAll(filepath.Join(dir, "data"), 0o755)
			life := setup(dir)
			a := importApp(t, dir, life, importAutonomyJSON)
			a.mu.Lock()
			a.loadMinds()
			err := a.mindsErr
			a.mu.Unlock()
			if err == nil {
				t.Fatal("应该报错")
			}
			if h := autonomyHealth(a); h.State != health.Error || !strings.Contains(h.Detail, "life.json") {
				t.Fatalf("健康条目：%+v", h)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "data", "life.json")); string(b) != life {
				t.Fatalf("life.json 被动了：%q", b)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "data", "autonomy.json")); string(b) != importAutonomyJSON {
				t.Fatalf("autonomy.json 被动了：%q", b)
			}
			if _, e := os.Stat(filepath.Join(dir, "data", "life.json.imported")); !os.IsNotExist(e) {
				t.Fatal("失败了不该有 life.json.imported")
			}
			// 自主活动不启动
			a.autonomyTick()
		})
	}
}

// 不是同一天的旧计数不导入（旧版换天就清零）；回访和找人记录照样导入。
func TestImportLifeSkipsYesterdayCounters(t *testing.T) {
	dir := t.TempDir()
	life := strings.Replace(importLifeJSON, `"day": "2026-10-21"`, `"day": "2026-10-20"`, 1)
	a := importApp(t, dir, life, "")
	a.mu.Lock()
	st := a.mind("default")
	w, r, tk := a.wakesToday(st, importDay), a.reachesToday(st, importDay), a.tokensToday(st, importDay)
	a.mu.Unlock()
	if w != 0 || r != 0 || tk != 0 {
		t.Fatalf("昨天的计数不该导入：%d %d %d", w, r, tk)
	}
	if len(st.FollowUps) != 1 || len(st.Awaiting) != 1 {
		t.Fatalf("回访和找人记录要导入：%+v", st)
	}
}
