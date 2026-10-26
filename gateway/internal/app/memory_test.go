package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/channel"
	"nano-agent/gateway/internal/knowledge"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
	"nano-agent/gateway/internal/skills"
)

func memWrite(t *testing.T, e *env, x knowledge.Entry) knowledge.Entry {
	t.Helper()
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	saved, err := e.a.memStore("default").Write(x)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func findMsg(msgs []llm.Message, substr string) string {
	for _, m := range msgs {
		if strings.Contains(m.Content, substr) {
			return m.Content
		}
	}
	return ""
}

// 记忆说明只在变了的时候放；想起来的同一条不重复提；群里看不到私密的记忆。
func TestMemoryNoteAndRecall(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	memWrite(t, e, knowledge.Entry{Path: "people/owner.md", Title: "主人", Type: "人物", Body: "喜欢喝手冲咖啡。", Visible: "admin"})
	memWrite(t, e, knowledge.Entry{Title: "国庆去成都", Type: "事件", Body: "主人国庆去成都吃了三次火锅。", Visible: "admin"})
	memWrite(t, e, knowledge.Entry{Title: "Go 泛型", Type: "知识", Body: "Go 1.18 起支持泛型。", Visible: "all"})

	e.say("我们聊聊成都火锅吧")
	e.k.expect(proto.TInput)
	got := lastUser(e.roundTrip(t))
	for _, want := range []string{memNoteOpen, "喜欢喝手冲咖啡", "国庆去成都", "Go 泛型", recallOpen, "三次火锅", stamp + " 我们聊聊成都火锅吧"} {
		if !strings.Contains(got, want) {
			t.Fatalf("第一轮里缺 %q：\n%s", want, got)
		}
	}
	if strings.Count(got, "people/owner.md") != 1 {
		t.Fatalf("对方的画像已经在记忆说明里，不该再“想起来”：\n%s", got)
	}

	// 记忆没变、想起来过的也提过了：第二轮什么都不加
	e.say("再说说成都火锅")
	e.k.expect(proto.TInput)
	got = lastUser(e.roundTrip(t))
	if strings.Contains(got, memNoteOpen) || strings.Contains(got, recallOpen) {
		t.Fatalf("记忆没变不该再放：\n%s", got)
	}

	// 记忆变了：下一轮重新放
	memWrite(t, e, knowledge.Entry{Title: "主人养猫", Type: "事实", Body: "主人养了一只橘猫。", Visible: "admin"})
	e.say("在吗")
	e.k.expect(proto.TInput)
	if got = lastUser(e.roundTrip(t)); !strings.Contains(got, "主人养猫") {
		t.Fatalf("记忆变了应该重新放：\n%s", got)
	}

	// 群里：只有公开的
	e.sid = "fake:g1"
	e.say("@机器人 成都火锅好吃吗", group(true), func(m *channel.Message) { m.ChatID = "g1" })
	e.k.expect(proto.TInput)
	msgs := e.roundTrip(t)
	note := findMsg(msgs, memNoteOpen)
	if note == "" || !strings.Contains(note, "Go 泛型") || strings.Contains(note, "国庆去成都") || strings.Contains(note, "手冲咖啡") {
		t.Fatalf("群里的记忆说明不对：\n%s", note)
	}
	if strings.Contains(findMsg(msgs, recallOpen), "三次火锅") {
		t.Fatal("群里不该想起私密的记忆")
	}
}

func knowledgeDo(t *testing.T, e *env, sid, who string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.knowledgeCall(sid, who, raw)
}

func TestKnowledgeTool(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	e.a.mu.Lock()
	e.a.store.SaveSettings("fake:g1", func() (st session.Settings) { st.Group = true; return }())
	e.a.mu.Unlock()

	out, err := knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "write", "title": "张三", "type": "人物",
		"summary": "产品经理", "body": "说话直，养了两只猫。", "ids": []string{"tg:123"}})
	if err != nil || !strings.Contains(out, "people/张三.md") {
		t.Fatalf("写人物：%q %v", out, err)
	}
	e.a.mu.Lock()
	p, ok := e.a.memStore("default").Person("tg:123", "")
	e.a.mu.Unlock()
	if !ok || p.Visible != "admin" || !strings.HasPrefix(p.Source, "main ") || p.Updated != "2026-10-21" {
		t.Fatalf("主人会话里写的默认只给主人看，带出处和日期：%+v", p)
	}
	if out, _ = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "search", "query": "产品经理"}); !strings.Contains(out, "张三") {
		t.Fatalf("搜不到：%q", out)
	}
	if _, err = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "edit", "path": "people/张三.md", "old": "两只猫", "new": "三只猫"}); err != nil {
		t.Fatal(err)
	}
	if out, _ = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "read", "path": "people/张三.md"}); !strings.Contains(out, "三只猫") {
		t.Fatalf("改了没生效：%q", out)
	}
	// 群里：看不到私密的，也不能写
	if out, _ = knowledgeDo(t, e, "fake:g1", "fake:u2", map[string]any{"action": "search", "query": "产品经理"}); strings.Contains(out, "张三") {
		t.Fatalf("群里不该搜到私密的：%q", out)
	}
	if _, err = knowledgeDo(t, e, "fake:g1", "fake:u2", map[string]any{"action": "write", "title": "x", "type": "事实", "body": "y"}); err == nil {
		t.Fatal("群里不能写记忆")
	}
	// 访客的私聊里写的只给他自己看，删不了
	if _, err = knowledgeDo(t, e, "fake:c9", "fake:u9", map[string]any{"action": "write", "title": "李四喜欢爬山", "type": "偏好", "body": "周末爬山", "visible": "all"}); err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	hits := e.a.memStore("default").Search("爬山", knowledge.SearchOptions{})
	e.a.mu.Unlock()
	if len(hits) != 1 || hits[0].Visible != "fake:u9" {
		t.Fatalf("访客写的只能自己看：%+v", hits)
	}
	if _, err = knowledgeDo(t, e, "fake:c9", "fake:u9", map[string]any{"action": "delete", "path": hits[0].Path}); err == nil {
		t.Fatal("访客不能真删")
	}
	// 标过时：索引里没了
	if _, err = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "invalidate", "path": "people/张三.md", "reason": "离职了"}); err != nil {
		t.Fatal(err)
	}
	if out, _ = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "list"}); strings.Contains(out, "张三") {
		t.Fatalf("过时的不该在索引里：%q", out)
	}
	// 路径越界
	if _, err = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "write", "path": "../../x.md", "title": "x", "type": "事实", "body": "y"}); err == nil {
		t.Fatal("越界的路径应该被拒")
	}
	// 自我认知
	if _, err = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "write", "path": "self.md", "body": "我叫小纳，喜欢看星星。"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, "agents", "default", "self.md")); !strings.Contains(string(b), "看星星") {
		t.Fatalf("self.md：%q", b)
	}
	// 共用的知识：主人会话能写，路径带 shared/
	if out, err = knowledgeDo(t, e, mainSID, "fake:u1", map[string]any{"action": "write", "path": "shared/规矩.md", "title": "规矩", "type": "流程", "body": "先测试再提交"}); err != nil || !strings.Contains(out, "shared/规矩.md") {
		t.Fatalf("共用知识：%q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "knowledge", "规矩.md")); err != nil {
		t.Fatal("共用知识应该写在 ~/.nano-agent/knowledge 下")
	}
}

// 关掉长期记忆：没有 Knowledge 工具，也不放记忆说明。
func TestMemoryOff(t *testing.T) {
	e := setup(t)
	store, _ := withAgents(t, e)
	memWrite(t, e, knowledge.Entry{Title: "Go 泛型", Type: "知识", Body: "Go 1.18 起支持泛型。", Visible: "all"})
	e.a.mu.Lock()
	ag, _ := store.Get("default")
	off := "off"
	ag.Memory = &off
	store.Update(ag)
	_, tools, _, _ := e.a.contextParts(mainSID)
	e.a.mu.Unlock()
	for _, tl := range tools {
		if tl.Function.Name == knowledgeLabel {
			t.Fatal("关了记忆还有 Knowledge 工具")
		}
	}
	e.say("聊聊 Go 泛型")
	e.k.expect(proto.TInput)
	if got := lastUser(e.roundTrip(t)); strings.Contains(got, memNoteOpen) {
		t.Fatalf("关了记忆还放记忆说明：%q", got)
	}
}

// agent 自己的技能和全局的合并，同名用自己的；dream、life 是保留的策略，不进 Skill 工具。
func TestAgentOwnSkills(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	mk := func(dir, name, desc, body string) {
		os.MkdirAll(filepath.Join(dir, name), 0o755)
		os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: "+desc+"\n---\n"+body), 0o644)
	}
	global := filepath.Join(e.dir, "skills")
	own := filepath.Join(e.dir, "agents", "default", "skills")
	mk(global, "notes", "全局的笔记", "全局正文")
	mk(global, "dream", "整理记忆", "全局的整理方法")
	mk(own, "notes", "我自己的笔记", "自己的正文")
	mk(own, "cook", "做饭", "菜谱")
	e.a.mu.Lock()
	e.a.refreshSkills()
	spec, ok := e.a.skillSpec(mainSID)
	text, _, _, loaded := skillsLoad(e.a, "notes")
	dream := e.a.skillText("default", "dream", "内置")
	e.a.mu.Unlock()
	if !ok || !strings.Contains(spec.Description, "我自己的笔记") || strings.Contains(spec.Description, "全局的笔记") || !strings.Contains(spec.Description, "cook") {
		t.Fatalf("合并后的技能：%s", spec.Description)
	}
	if strings.Contains(spec.Description, "dream") {
		t.Fatal("dream 是保留的，不该出现在 Skill 工具里")
	}
	if !loaded || !strings.Contains(text, "自己的正文") {
		t.Fatalf("同名应该用 agent 自己的：%q", text)
	}
	if dream != "全局的整理方法" {
		t.Fatalf("保留技能的正文：%q", dream)
	}
}

func skillsLoad(a *App, name string) (string, []byte, string, bool) {
	text, _, hash, ok := skills.LoadAny(name, a.ownSkills("default"), a.skills)
	return text, nil, hash, ok
}

// 已有聊天发现新增/修改/删除的技能，不要求 /reset；正文变化仍在加载时读取。
func TestSkillCatalogRefreshesExistingConversation(t *testing.T) {
	e := setup(t)
	e.say("保留这段原有聊天")
	e.k.expect(proto.TInput)
	e.roundTrip(t)
	if hasTool(e.llm.lastReq().Tools, "Skill") {
		t.Fatal("初始目录不应有技能")
	}
	dir := filepath.Join(e.dir, "skills", "workflow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct{ description, body string }{{"任务续做方法", "第一版"}, {"更新后的任务方法", "第二版"}} {
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: workflow\ndescription: "+step.description+"\n---\n"+step.body), 0o644); err != nil {
			t.Fatal(err)
		}
		e.say("继续用可用技能")
		e.k.expect(proto.TInput)
		e.roundTrip(t)
		request := e.llm.lastReq()
		found := false
		for _, tool := range request.Tools {
			if tool.Function.Name == "Skill" && strings.Contains(tool.Function.Description, step.description) && strings.Contains(string(tool.Function.Parameters), "workflow") {
				found = true
			}
		}
		if !found {
			t.Fatalf("已有会话没有刷新技能目录: %+v", request.Tools)
		}
		e.a.mu.Lock()
		text, _, _, loaded := skillsLoad(e.a, "workflow")
		e.a.mu.Unlock()
		if !loaded || !strings.Contains(text, step.body) {
			t.Fatalf("没加载最新正文: %s", text)
		}
	}
	if err := os.Remove(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	e.say("再次继续")
	e.k.expect(proto.TInput)
	messages := e.roundTrip(t)
	if hasTool(e.llm.lastReq().Tools, "Skill") {
		t.Fatal("已删除的技能仍留在旧工具快照里")
	}
	found := false
	for _, message := range messages {
		found = found || strings.Contains(message.Content, "保留这段原有聊天")
	}
	if !found {
		t.Fatal("刷新技能不应清空聊天历史")
	}
}

// 网页上的记忆：列出、新建、标过时再恢复、删除。
func TestMemoryWeb(t *testing.T) {
	e := setup(t)
	withAgents(t, e)
	item, err := e.a.MemorySave("default", "", "---\ntitle: 王五\ntype: 人物\n---\n大学同学")
	if err != nil || item.Path != "people/王五.md" {
		t.Fatalf("新建：%+v %v", item, err)
	}
	if err := e.a.MemoryInvalidate("default", item.Path, "", false); err != nil {
		t.Fatal(err)
	}
	v, _ := e.a.Memory("default")
	if v.Total != 1 || v.People != 1 || v.Invalid != 1 || v.Entries[0].Invalid == "" {
		t.Fatalf("概况：%+v", v)
	}
	if err := e.a.MemoryInvalidate("default", item.Path, "", true); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.a.Memory("default"); v.Invalid != 0 {
		t.Fatalf("恢复后：%+v", v)
	}
	if hits, _ := e.a.MemorySearch("default", "同学"); len(hits) != 1 {
		t.Fatalf("搜索：%+v", hits)
	}
	if err := e.a.MemoryDelete("default", item.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.MemoryRaw("default", item.Path); err == nil {
		t.Fatal("删了还能读")
	}
	if _, err := e.a.MemoryRaw("default", "../agents.json"); err == nil {
		t.Fatal("越界的路径")
	}
}
