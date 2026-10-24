package app

import (
	"errors"
	"strings"

	"nano-agent/gateway/internal/knowledge"
)

// 网页「记忆」节点用的接口：每个 agent 的知识库、人物画像、自我认知，能看能搜能改能删。
// 路径带 shared/ 的是所有 agent 共用的知识（和 Knowledge 工具一样）。

// MemoryItem 是网页列表里的一条记忆（不带正文，点开再读原文）。
type MemoryItem struct {
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Type    string   `json:"type"`
	Line    string   `json:"line"`
	People  []string `json:"people,omitempty"`
	Visible string   `json:"visible,omitempty"`
	Source  string   `json:"source,omitempty"`
	Updated string   `json:"updated,omitempty"`
	Invalid string   `json:"invalid,omitempty"`
	Shared  bool     `json:"shared,omitempty"`
	Snippet string   `json:"snippet,omitempty"`
}

// MemoryView 是一个 agent 的记忆概况。
type MemoryView struct {
	Agent   string       `json:"agent"`
	On      bool         `json:"on"`
	Self    string       `json:"self"`
	Entries []MemoryItem `json:"entries"`
	Total   int          `json:"total"`
	People  int          `json:"people"`
	Invalid int          `json:"invalid"`
	Dir     string       `json:"dir"`
}

func memItem(e knowledge.Entry, shared bool) MemoryItem {
	p := e.Path
	if shared {
		p = sharedPrefix + p
	}
	return MemoryItem{Path: p, Title: e.Title, Type: e.Type, Line: e.Line(), People: e.People, Visible: e.Visible,
		Source: e.Source, Updated: e.Updated, Invalid: e.Invalid, Shared: shared}
}

// memTarget 找到路径对应的知识库。调用方持有锁。
func (a *App) memTarget(agentID, path string) (*knowledge.Store, string, error) {
	if _, ok := a.agentByID(agentID); !ok {
		return nil, "", errors.New("没有这个 agent")
	}
	if a.agentRoot == "" {
		return nil, "", errors.New("长期记忆还没准备好")
	}
	if rest, ok := strings.CutPrefix(strings.TrimSpace(path), sharedPrefix); ok {
		return a.sharedMem(), rest, nil
	}
	return a.memStore(agentID), strings.TrimSpace(path), nil
}

// Memory 是一个 agent 的记忆（网页用）。
func (a *App) Memory(agentID string) (MemoryView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ag, ok := a.agentByID(agentID)
	if !ok || a.agentRoot == "" {
		return MemoryView{}, errors.New("没有这个 agent")
	}
	v := MemoryView{Agent: ag.ID, On: a.memoryOn(ag), Self: a.selfText(ag.ID), Entries: []MemoryItem{}, Dir: "agents/" + ag.ID + "/knowledge"}
	for _, e := range a.memStore(ag.ID).List() {
		v.Entries = append(v.Entries, memItem(e, false))
		v.Total++
		if e.Type == "人物" {
			v.People++
		}
		if e.Invalid != "" {
			v.Invalid++
		}
	}
	for _, e := range a.sharedMem().List() {
		v.Entries = append(v.Entries, memItem(e, true))
	}
	return v, nil
}

// MemorySearch 在一个 agent 的记忆和共用知识里搜（连过时的也搜，网页上灰着显示）。
func (a *App) MemorySearch(agentID, q string) ([]MemoryItem, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.agentByID(agentID); !ok || a.agentRoot == "" {
		return nil, errors.New("没有这个 agent")
	}
	o := knowledge.SearchOptions{IncludeInvalid: true, Limit: 30}
	out := []MemoryItem{}
	for _, h := range a.memStore(agentID).Search(q, o) {
		it := memItem(h.Entry, false)
		it.Snippet = h.Snippet
		out = append(out, it)
	}
	for _, h := range a.sharedMem().Search(q, o) {
		it := memItem(h.Entry, true)
		it.Snippet = h.Snippet
		out = append(out, it)
	}
	return out, nil
}

// MemoryRaw 是一条记忆的原文（含 frontmatter）。
func (a *App) MemoryRaw(agentID, path string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, rel, err := a.memTarget(agentID, path)
	if err != nil {
		return "", err
	}
	return st.Raw(rel)
}

// MemorySave 用原文保存一条记忆；path 为空就按标题新建。
func (a *App) MemorySave(agentID, path, raw string) (MemoryItem, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, rel, err := a.memTarget(agentID, path)
	if err != nil {
		return MemoryItem{}, err
	}
	shared := st == a.sharedMem()
	var e knowledge.Entry
	if rel == "" {
		e, err = st.Write(knowledge.Parse("", raw))
	} else {
		e, err = st.WriteRaw(rel, raw)
	}
	if err != nil {
		return MemoryItem{}, err
	}
	a.publishMemory(agentID)
	return memItem(e, shared), nil
}

// MemoryInvalidate 标一条记忆过时，restore 为真时恢复。
func (a *App) MemoryInvalidate(agentID, path, reason string, restore bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, rel, err := a.memTarget(agentID, path)
	if err != nil {
		return err
	}
	if restore {
		e, ok := st.Get(rel)
		if !ok {
			return errors.New("没有这条记忆")
		}
		e.Invalid = ""
		_, err = st.Write(e)
	} else {
		if strings.TrimSpace(reason) == "" {
			reason = "主人在网页上标的"
		}
		_, err = st.Invalidate(rel, reason, a.now().In(a.loc).Format("2006-01-02"))
	}
	if err == nil {
		a.publishMemory(agentID)
	}
	return err
}

// MemoryDelete 真删一条记忆。
func (a *App) MemoryDelete(agentID, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, rel, err := a.memTarget(agentID, path)
	if err != nil {
		return err
	}
	if err := st.Delete(rel); err != nil {
		return err
	}
	a.publishMemory(agentID)
	return nil
}

// SaveMemorySelf 保存 agent 的自我认知（self.md）。
func (a *App) SaveMemorySelf(agentID, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.agentByID(agentID); !ok || a.agentRoot == "" {
		return errors.New("没有这个 agent")
	}
	return a.writeSelf(agentID, text)
}
