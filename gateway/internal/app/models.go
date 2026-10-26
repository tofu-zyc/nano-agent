package app

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/agents"
	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/models"
)

// 模型：可以配好几个（models 包），一个默认，每个会话可以单独选（/model、网页）。
// 思考强度的可选值、历史上限跟着模型走；模型客户端按需创建并按配置缓存。
// 没有接模型列表时（测试/嵌入式），退回旧版单模型模式，用构造时传进来的客户端。

// SetModels 接上模型列表。newClient 按设置建模型客户端。没接时（测试）所有会话都用 a.llm。
func (a *App) SetModels(s *models.Store, newClient func(config.LLM) (llm.Client, error)) {
	a.mu.Lock()
	a.models, a.newClient = s, newClient
	a.mu.Unlock()
}

type modelClient struct {
	cfg    config.LLM
	client llm.Client
}

// modelFor 是一个 agent 用的模型：agent 指定了就用它，否则用默认模型。调用方持有锁。
func (a *App) modelFor(ag agents.Agent) (models.Model, models.Endpoint, bool) {
	if a.models == nil {
		return legacyModel(a.cfg.LLM)
	}
	if ag.Model != "" {
		if m, e, ok := a.models.Find(ag.Model); ok {
			return m, e, true
		}
	}
	return a.models.Find(a.models.Default())
}

// modelOf 是会话用的模型：会话自己选的优先，其次是所属 agent 的，最后是默认模型；
// 没有模型列表或者找不到时 ok 为 false。调用方持有锁。
// 没接模型列表时（测试/嵌入式）用旧的单模型配置凑一个，网关本身总是接 models.Store。
func (a *App) modelOf(sid string) (models.Model, models.Endpoint, bool) {
	if a.models == nil {
		return legacyModel(a.cfg.LLM)
	}
	if id := a.store.Settings(sid).Model; id != "" {
		if m, e, ok := a.models.Find(id); ok {
			return m, e, true
		}
	}
	return a.modelFor(a.agentOf(sid))
}

// legacyModel 是没接模型列表时的单模型模式（只给测试/嵌入式用）。
func legacyModel(l config.LLM) (models.Model, models.Endpoint, bool) {
	name := strings.TrimSpace(l.Model)
	if name == "" && l.Provider == "mock" {
		name = "mock"
	}
	if name == "" {
		return models.Model{}, models.Endpoint{}, false
	}
	return models.Model{ID: models.ConfigEndpoint + "/" + name, Endpoint: models.ConfigEndpoint, Name: name,
			ContextWindow: l.ContextWindow, MaxTokens: l.MaxTokens, MaxHistory: l.MaxHistoryTokens,
			Thinking: l.Thinking, ThinkingLevels: l.ThinkingLevels, Modalities: l.Modalities},
		models.Endpoint{ID: models.ConfigEndpoint, Name: "模型接口", Kind: l.Provider, BaseURL: l.BaseURL, APIKey: l.APIKey, Proxy: l.Proxy}, true
}

// clientFor 是会话这一轮用的模型客户端。调用方持有锁。
func (a *App) clientFor(sid string) (llm.Client, error) {
	m, e, ok := a.modelOf(sid)
	if !ok {
		return nil, errors.New("还没有配置模型")
	}
	if a.models == nil || a.newClient == nil {
		return a.llm, nil
	}
	cfg := a.models.LLMConfig(m, e)
	if c, ok := a.clients[m.ID]; ok && reflect.DeepEqual(c.cfg, cfg) {
		return c.client, nil
	}
	c, err := a.newClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("模型 %s 的设置有误：%w", m.ID, err)
	}
	a.clients[m.ID] = modelClient{cfg: cfg, client: c}
	return c, nil
}

// levelsOf 是会话实际生效的模型可选的思考强度：会话选的模型，其次是当前 agent 的默认模型。
func (a *App) levelsOf(sid string) []string {
	return a.levelsForAgentModel(a.execAgentID(sid), a.store.Settings(sid).Model)
}

// levelsForAgentModel 是 agent 用模型 modelID 时的思考强度可选值；modelID 为空表示用 agent 的默认模型。
func (a *App) levelsForAgentModel(agentID, modelID string) []string {
	if modelID == "" {
		if ag, ok := a.agentByID(agentID); ok {
			modelID = ag.Model
		}
	}
	return a.levelsForModel(modelID)
}

// levelsForModel 是模型 id 的可选思考强度：空表示默认模型，找不到时用默认模型的
func (a *App) levelsForModel(id string) []string {
	if a.models == nil {
		return a.cfg.LLM.ThinkingLevels
	}
	if id != "" {
		if m, _, ok := a.models.Find(id); ok {
			return m.ThinkingLevels
		}
	}
	if m, _, ok := a.models.Find(a.models.Default()); ok {
		return m.ThinkingLevels
	}
	return nil
}

// modelExists 判断这个模型在不在列表里
func (a *App) modelExists(id string) bool {
	if a.models == nil {
		return false
	}
	_, _, ok := a.models.Find(id)
	return ok
}

// 模型没填上下文窗口时的兜底历史上限（正常情况下创建模型时必填）。
const defaultHistoryLimit = 128000

// historyLimit 是会话的历史上限（估算的 token 数）：模型设了就用模型的；
// 没设时按上下文窗口的一半，留一半给工具输出和回复。
func (a *App) historyLimit(sid string) int {
	if a.models == nil {
		if a.cfg.LLM.MaxHistoryTokens > 0 {
			return a.cfg.LLM.MaxHistoryTokens
		}
		return defaultHistoryLimit
	}
	m, _, ok := a.modelOf(sid)
	if !ok {
		return defaultHistoryLimit
	}
	if m.MaxHistory > 0 {
		return m.MaxHistory
	}
	if m.ContextWindow > 0 {
		return m.ContextWindow / 2
	}
	return defaultHistoryLimit
}

func modelLabel(m models.Model) string {
	if m.Label != "" {
		return m.Label
	}
	return m.Name
}

// ModelStore 是模型列表（没接时为 nil）
func (a *App) ModelStore() *models.Store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.models
}

// ClientFor 是请求模型 id 用的客户端（网页上测试模型用），id 为空表示默认模型
func (a *App) ClientFor(id string) (llm.Client, models.Model, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.models == nil {
		m, _, ok := legacyModel(a.cfg.LLM)
		if !ok {
			return nil, m, errors.New("还没有配置模型")
		}
		return a.llm, m, nil
	}
	if id == "" {
		id = a.models.Default()
	}
	if id == "" {
		return nil, models.Model{}, errors.New("还没有配置模型")
	}
	m, e, ok := a.models.Find(id)
	if !ok {
		return nil, m, fmt.Errorf("没有模型 %s", id)
	}
	if a.newClient == nil {
		return a.llm, m, nil
	}
	c, err := a.newClient(a.models.LLMConfig(m, e))
	return c, m, err
}

// ModelsChanged：网页里改了模型列表。删掉了模型的会话改回默认模型，所有会话都刷新一遍
// （默认模型变了，没单独选模型的会话也跟着变）。
func (a *App) ModelsChanged() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.publish(events.Event{T: "models"})
	for _, sid := range a.sessionIDs() {
		st := a.store.Settings(sid)
		if st.Model != "" && !a.modelExists(st.Model) {
			st.Model = ""
			if err := a.store.SaveSettings(sid, st); err != nil {
				a.log.Warn("保存会话设置失败", "sid", sid, "err", err)
			}
		}
		a.publishSession(sid)
	}
}

// ---- /model ----

func (a *App) cmdModel(c in, arg string) {
	if a.models == nil {
		a.answer(c, "模型："+a.cfg.LLM.Model)
		return
	}
	list := a.models.Models()
	if len(list) == 0 {
		a.answer(c, "还没有配置模型，去网页的模型节点里添加接口和模型")
		return
	}
	cur, _, _ := a.modelOf(c.s.sid)
	if arg == "" {
		var b strings.Builder
		b.WriteString("这个会话用的模型：" + modelLabel(cur) + "\n可以用的模型：\n")
		def := a.models.Default()
		for i, m := range list {
			var tags []string
			if m.ID == cur.ID {
				tags = append(tags, "当前")
			}
			if m.ID == def {
				tags = append(tags, "默认")
			}
			if slices.Contains(m.Modalities, "image") {
				tags = append(tags, "图片")
			}
			fmt.Fprintf(&b, "%d. %s（%s）", i+1, modelLabel(m), m.ID)
			if len(tags) > 0 {
				b.WriteString(" · " + strings.Join(tags, "，"))
			}
			b.WriteString("\n")
		}
		b.WriteString("/model <编号> 切换，/model default 用默认的模型")
		a.answer(c, b.String())
		return
	}
	if !a.isAdmin(c.who) {
		a.answer(c, "只有管理员能切换模型")
		return
	}
	id := ""
	if arg != "default" {
		n, err := strconv.Atoi(arg)
		if err == nil && n >= 1 && n <= len(list) {
			id = list[n-1].ID
		} else if _, _, ok := a.models.Find(arg); ok {
			id = arg
		} else {
			a.answer(c, "没有这个模型，用 /model 看看有哪些")
			return
		}
	}
	if err := a.setSessionModel(c.s.sid, id); err != nil {
		a.answer(c, err.Error())
		return
	}
	m, _, _ := a.modelOf(c.s.sid)
	a.answer(c, "这个会话改用 "+modelLabel(m)+"，下一条消息开始生效")
}

// setSessionModel 让会话用模型 id（空表示默认模型）。思考强度不在新模型的可选值里时恢复默认。调用方持有锁。
func (a *App) setSessionModel(sid, id string) error {
	if a.models == nil {
		return errors.New("模型列表没有启用")
	}
	if id != "" && !a.modelExists(id) {
		return fmt.Errorf("没有模型 %s", id)
	}
	st := a.store.Settings(sid)
	st.Model = id
	if err := a.store.SaveSettings(sid, st); err != nil {
		return err
	}
	if t := st.Thinking; t != "" && !contains(a.levelsOf(sid), t) {
		st.Thinking = ""
		a.store.SaveSettings(sid, st)
	}
	a.publishSession(sid)
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
