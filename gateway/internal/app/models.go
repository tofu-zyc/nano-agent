package app

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/events"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/models"
)

// 模型：可以配好几个（models 包），一个默认，每个会话可以单独选（/model、网页）。
// 思考强度的可选值、历史上限跟着模型走。配置文件里 [llm] 的那个模型用 a.llm（换配置时由 SetLLM 换掉），
// 别的模型按需建客户端，配置变了重建。

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

// modelOf 是会话用的模型；没有模型列表或者找不到时 ok 为 false（用配置文件里的）
func (a *App) modelOf(sid string) (models.Model, models.Endpoint, bool) {
	if a.models == nil {
		return models.Model{}, models.Endpoint{}, false
	}
	id := a.store.Settings(sid).Model
	if id != "" {
		if m, e, ok := a.models.Find(id); ok {
			return m, e, true
		}
	}
	return a.models.Find(a.models.Default())
}

// clientFor 是会话这一轮用的模型客户端。调用方持有锁。
func (a *App) clientFor(sid string) (llm.Client, error) {
	m, e, ok := a.modelOf(sid)
	if !ok || e.ID == models.ConfigEndpoint || a.newClient == nil {
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

// levelsOf 是会话的模型可选的思考强度
func (a *App) levelsOf(sid string) []string {
	return a.levelsForModel(a.store.Settings(sid).Model)
}

// levelsForModel 是模型 id 的可选思考强度：空表示默认模型，找不到时用配置里的
func (a *App) levelsForModel(id string) []string {
	if a.models != nil {
		if id != "" {
			if m, _, ok := a.models.Find(id); ok {
				return m.ThinkingLevels
			}
		}
		if m, _, ok := a.models.Find(a.models.Default()); ok {
			return m.ThinkingLevels
		}
	}
	return a.cfg.LLM.ThinkingLevels
}

// modelExists 判断这个模型在不在列表里
func (a *App) modelExists(id string) bool {
	if a.models == nil {
		return false
	}
	_, _, ok := a.models.Find(id)
	return ok
}

// historyLimit 是会话的历史上限（估算的 token 数）：模型设了就用模型的，否则用配置里的
func (a *App) historyLimit(sid string) int {
	if m, _, ok := a.modelOf(sid); ok && m.MaxHistory > 0 {
		return m.MaxHistory
	}
	if m, _, ok := a.modelOf(sid); ok && m.ContextWindow > 0 && m.Endpoint != models.ConfigEndpoint {
		return m.ContextWindow / 2 // 留一半给工具输出和回复
	}
	return a.cfg.LLM.MaxHistoryTokens
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
		return a.llm, models.Model{Name: a.cfg.LLM.Model, Thinking: a.cfg.LLM.Thinking}, nil
	}
	if id == "" {
		id = a.models.Default()
	}
	m, e, ok := a.models.Find(id)
	if !ok {
		return nil, m, fmt.Errorf("没有模型 %s", id)
	}
	if e.ID == models.ConfigEndpoint || a.newClient == nil {
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
