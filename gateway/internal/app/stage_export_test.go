package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/scene"
)

// 导出（设计 §14.3）：字幕和剧本有逐字节的快照；长句拆分、时长上下限、开关的组合另有测试。

func TestSplitSentences(t *testing.T) {
	got := splitSentences("我昨晚看到他在书房。你们怎么看？太可怕了！还没写完")
	want := []string{"我昨晚看到他在书房。", "你们怎么看？", "太可怕了！", "还没写完"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
}

func TestSRTTimingAndSplit(t *testing.T) {
	long := "第一句话说得比较长比较长比较长比较长。第二句也不短不短不短不短不短？第三句同样很长很长很长很长很长！"
	if runeLen(long) <= srtSplitOver {
		t.Fatal("测试数据要超过 40 字")
	}
	items := []docItem{
		{Kind: "say", Who: "小鲍", Text: "好"},                     // 太短：1.5 秒
		{Kind: "narrate", Text: strings.Repeat("长", 39)},        // 不拆；39 字 → 7.8 秒
		{Kind: "say", Who: "小猫", Text: strings.Repeat("字", 41)}, // 超过 40 字但没有标点：不拆，封顶 8 秒
		{Kind: "say", Who: "小纳", Text: long},                    // 拆成三条，名字只在第一条
		{Kind: "narrate", Text: "x\n y  z"},                     // 空白合并
	}
	got := renderSRT(items, ExportOpts{})
	want := "1\n00:00:00,000 --> 00:00:01,500\n小鲍：好\n\n" +
		"2\n00:00:01,800 --> 00:00:09,600\n" + strings.Repeat("长", 39) + "\n\n" +
		"3\n00:00:09,900 --> 00:00:17,900\n小猫：" + strings.Repeat("字", 41) + "\n\n"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("前三条：\n%s\n应为\n%s", got, want)
	}
	rest := strings.TrimPrefix(got, want)
	parts := strings.Split(strings.TrimSpace(rest), "\n\n")
	if len(parts) != 4 { // 三条拆句 + 最后一条旁白
		t.Fatalf("长句拆成三条，加最后一条：%d\n%s", len(parts), rest)
	}
	if !strings.Contains(parts[0], "小纳：第一句话") || strings.Contains(parts[1], "小纳") || strings.Contains(parts[2], "小纳") {
		t.Fatalf("名字只在第一条：\n%s", rest)
	}
	if !strings.HasSuffix(parts[3], "x y z") {
		t.Fatalf("空白合并：%q", parts[3])
	}
}

func TestSRTSwitches(t *testing.T) {
	items := []docItem{
		{Kind: "inner", Who: "小鲍", Text: "他在撒谎", Line: 2},
		{Kind: "say", Who: "小鲍", Text: "我同意", Line: 2},
		{Kind: "say", Who: "小猫", Channel: "密谈", Text: "只告诉你", Line: 3},
		{Kind: "inner", Who: "小猫", Channel: "密谈", Text: "他不信", Line: 3},
		{Kind: "narrate", Channel: "密谈", Text: "烛光摇曳", Line: 4},
		{Kind: "cue", To: "小鲍", Text: "走到窗边", Line: 5},
		{Kind: "system", Text: "先暂停", Line: 6},
	}
	has := func(s, sub string) bool { return strings.Contains(s, sub) }
	// 什么开关都不开：只有公开发言
	s0 := renderSRT(items, ExportOpts{})
	if !has(s0, "小鲍：我同意") || has(s0, "心里") || has(s0, "私下") || has(s0, "窗边") || has(s0, "暂停") {
		t.Fatalf("默认：\n%s", s0)
	}
	// 心里话在那句话前面；cue 和系统提示永远不进字幕
	s1 := renderSRT(items, ExportOpts{Inner: true})
	if !has(s1, "（小鲍心里）他在撒谎") || strings.Index(s1, "心里") > strings.Index(s1, "我同意") || has(s1, "他不信") || has(s1, "私下") {
		t.Fatalf("inner：\n%s", s1)
	}
	s2 := renderSRT(items, ExportOpts{Channels: true})
	if !has(s2, "小猫（私下）：只告诉你") || !has(s2, "（私下）烛光摇曳") || has(s2, "心里") || has(s2, "窗边") || has(s2, "暂停") {
		t.Fatalf("channels：\n%s", s2)
	}
	s3 := renderSRT(items, ExportOpts{Inner: true, Channels: true})
	if !has(s3, "（小猫心里）他不信") || has(s3, "窗边") || has(s3, "暂停") {
		t.Fatalf("两个都开：\n%s", s3)
	}
}

// 同一个脚本场导出的 SRT 和 Markdown 有逐字节的快照。
func TestStageExportGolden(t *testing.T) {
	e := setup(t)
	m := &scriptModel{reply: func(who string, n int) (string, error) {
		if who == "bob" {
			return "<心里>他在撒谎</心里>我觉得管家有问题。我昨晚看到他在书房。你们怎么看？这是一个很长很长很长很长很长很长的句子，用来测试拆分。", nil
		}
		return "<心里>先观察</心里>[不回复]", nil
	}}
	e.a.SetLLM(m)
	room, _ := threeRoom(t, e, stageRules(scene.Rules{Mode: scene.ModeDirector, Inner: true, Brief: "晚宴上有人被毒死。",
		Roles: map[string]scene.Role{"bob": {Prompt: "你是侦探。"}}}))
	say := func(text, as, ch string) {
		if err := e.a.RoomSay(room.ID, text, as, ch, "我"); err != nil {
			t.Fatal(err)
		}
	}
	say("夜深了，庄园里的灯一盏盏熄灭。", "旁白", "")
	say("是谁干的？", "张三", "")
	say("小鲍、小猫，我只告诉你们。", "张三", "s")
	if err := e.a.RoomCue(room.ID, "bob", "走到窗边", ""); err != nil {
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("bob", room.ID))
	if err := e.a.RoomCue(room.ID, "default", "", ""); err != nil { // 想了但没说话
		t.Fatal(err)
	}
	step(t, e, e.a.roomMemberSID("default", room.ID))
	e.a.mu.Lock()
	r, _ := e.a.roomStore.Get(room.ID)
	e.a.pauseRoom(r, "测试暂停")
	e.a.mu.Unlock()

	check := func(name string, o ExportOpts) string {
		t.Helper()
		data, _, fn, err := e.a.ExportStage(room.ID, o)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(fn, "."+o.Format) {
			t.Fatalf("文件名：%s", fn)
		}
		path := filepath.Join("testdata", "stage-export", name)
		old, rerr := os.ReadFile(path)
		if rerr == nil && string(old) == string(data) {
			return string(data)
		}
		if !*updateSurface {
			t.Fatalf("%s 和快照不同（%v）。确认是有意的之后：go test ./internal/app -run StageExportGolden -update\n%s", name, rerr, data)
		}
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	check("plain.srt", ExportOpts{Format: "srt"})
	check("full.srt", ExportOpts{Format: "srt", Inner: true, Channels: true})
	check("plain.md", ExportOpts{Format: "md"})
	full := check("full.md", ExportOpts{Format: "md", Inner: true, Channels: true})
	check("range.srt", ExportOpts{Format: "srt", Inner: true, From: 2, To: 3})
	if !strings.Contains(full, "共 ") || !strings.Contains(full, "*（小鲍心里）他在撒谎*") {
		t.Fatalf("剧本要有心里话和末尾的用量：\n%s", full)
	}
	// 范围：只有第 2、3 句
	rng := check("range.srt", ExportOpts{Format: "srt", Inner: true, From: 2, To: 3})
	if strings.Contains(rng, "夜深了") || !strings.Contains(rng, "是谁干的") {
		t.Fatalf("范围：\n%s", rng)
	}
	// json：范围内的记录，加上成员、规则和用量
	data, ctype, _, err := e.a.ExportStage(room.ID, ExportOpts{Format: "json", From: 2, To: 3})
	if err != nil || !strings.HasPrefix(ctype, "application/json") {
		t.Fatalf("%v %s", err, ctype)
	}
	var got struct {
		Room    map[string]any `json:"room"`
		Use     map[string]any `json:"use"`
		Records []StageRec     `json:"records"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, r := range got.Records {
		if r.Kind == "line" && (r.Line < 2 || r.Line > 3) {
			t.Fatalf("范围外的句子：%+v", r)
		}
		if r.Kind == "prefix" && r.Body != "" {
			t.Fatal("prefix 只给哈希和长度")
		}
	}
	if got.Room["name"] != "群" || got.Use == nil || len(got.Records) == 0 {
		t.Fatalf("json：%s", data)
	}
	// 参数错误
	for _, bad := range []ExportOpts{{Format: "doc"}, {Format: "srt", From: 5, To: 2}, {Format: "srt", From: -1}} {
		if _, _, _, err := e.a.ExportStage(room.ID, bad); err == nil {
			t.Errorf("%+v 应该报错", bad)
		}
	}
}
