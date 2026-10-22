package app

import (
	"strings"
	"sync"
	"testing"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/proto"
)

// 接上模型列表以后：/model 列出和切换模型，切换后这一轮用新模型的客户端；
// 原来的思考强度在新模型上没有时恢复默认；历史上限跟着模型走。
func TestModelSwitchPerSession(t *testing.T) {
	e := setup(t)
	ms, err := models.Open(e.dir, e.a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var clients []*recLLM
	e.a.SetModels(ms, func(config.LLM) (llm.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		c := &recLLM{}
		clients = append(clients, c)
		return c, nil
	})
	clientCount := func() int { mu.Lock(); defer mu.Unlock(); return len(clients) }
	lastClient := func() *recLLM {
		mu.Lock()
		defer mu.Unlock()
		if len(clients) == 0 {
			return nil
		}
		return clients[len(clients)-1]
	}
	ep, err := ms.SaveEndpoint(models.Endpoint{Name: "另一个接口", Kind: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ms.SaveModel(models.Model{Endpoint: ep.ID, Name: "fast", Label: "Fast",
		ContextWindow: 200000, ThinkingLevels: []string{"none", "high"}})
	if err != nil {
		t.Fatal(err)
	}

	// 没选模型时用配置文件里的（a.llm），不另建客户端
	e.say("先聊一句")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）收到：先聊一句")
	if clientCount() != 0 {
		t.Fatalf("没选模型时不该建别的客户端：%d", clientCount())
	}
	if got := e.a.historyLimit(mainSID); got != e.a.cfg.LLM.MaxHistoryTokens {
		t.Fatalf("配置文件模型的历史上限：%d", got)
	}

	// /model 能列出两个模型
	e.say("/model")
	if list := e.ch.wait(t, "可以用的模型"); !strings.Contains(list, "Fast") {
		t.Fatalf("/model 没列出新模型：%s", list)
	}

	// 原来的思考强度不在新模型的可选值里：切过去以后恢复默认
	st := e.a.store.Settings(mainSID)
	st.Thinking = "max"
	e.a.store.SaveSettings(mainSID, st)

	e.say("/model 2")
	e.ch.wait(t, "这个会话改用 Fast")
	if got := e.a.store.Settings(mainSID); got.Model != m.ID || got.Thinking != "" {
		t.Fatalf("切换后的设置：model=%q thinking=%q", got.Model, got.Thinking)
	}
	if got := e.a.historyLimit(mainSID); got != 100000 {
		t.Fatalf("新模型的历史上限：%d", got)
	}

	// 这一轮用新模型的客户端
	e.say("换模型之后")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	e.ch.wait(t, "（mock）收到：换模型之后")
	c := lastClient()
	if c == nil {
		t.Fatal("没有用会话选的模型")
	}
	if req := c.lastReq(); len(req.Messages) == 0 {
		t.Fatal("新模型的客户端没有收到请求")
	}
}

// 模型被删掉以后，接着它的会话改回默认模型，不留下不存在的选择。
func TestModelDeletedFallsBack(t *testing.T) {
	e := setup(t)
	ms, err := models.Open(e.dir, e.a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetModels(ms, func(config.LLM) (llm.Client, error) { return &recLLM{}, nil })
	ep, err := ms.SaveEndpoint(models.Endpoint{Name: "别的", Kind: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ms.SaveModel(models.Model{Endpoint: ep.ID, Name: "m"})
	if err != nil {
		t.Fatal(err)
	}
	st := e.a.store.Settings(mainSID)
	st.Model = m.ID
	e.a.store.SaveSettings(mainSID, st)

	if err := ms.DeleteModel(m.ID); err != nil {
		t.Fatal(err)
	}
	e.a.ModelsChanged()
	if got := e.a.store.Settings(mainSID).Model; got != "" {
		t.Fatalf("删掉模型以后会话还在用它：%q", got)
	}
	if _, _, ok := e.a.modelOf(mainSID); !ok {
		t.Fatal("应该回退到默认模型")
	}
}

// 会话设置里能单独选模型；没有模型列表时直接说不支持。
func TestUpdateSessionModel(t *testing.T) {
	e := setup(t)
	ms, err := models.Open(e.dir, e.a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetModels(ms, func(config.LLM) (llm.Client, error) { return &recLLM{}, nil })
	ep, err := ms.SaveEndpoint(models.Endpoint{Name: "别的", Kind: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ms.SaveModel(models.Model{Endpoint: ep.ID, Name: "m", ThinkingLevels: []string{"low", "high"}})
	if err != nil {
		t.Fatal(err)
	}
	model := m.ID
	if err := e.a.UpdateSession(mainSID, SessionPatch{Model: &model}); err != nil {
		t.Fatal(err)
	}
	info, _ := e.a.Session(mainSID)
	if info.Model != m.ID || info.ModelUsed != m.ID || info.ModelLabel != "m" {
		t.Fatalf("会话信息：%+v", info)
	}
	if len(info.ThinkingLevels) != 2 {
		t.Fatalf("思考强度可选值：%v", info.ThinkingLevels)
	}
	// 不存在的模型要拒绝
	bad := "nope"
	if err := e.a.UpdateSession(mainSID, SessionPatch{Model: &bad}); err == nil {
		t.Fatal("不存在的模型应该报错")
	}
	// 切回默认
	empty := ""
	if err := e.a.UpdateSession(mainSID, SessionPatch{Model: &empty}); err != nil {
		t.Fatal(err)
	}
	if info, _ := e.a.Session(mainSID); info.Model != "" {
		t.Fatalf("切回默认以后 model=%q", info.Model)
	}
}
