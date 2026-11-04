package app

import (
	"errors"
	"path/filepath"
	"slices"

	"nano-agent/gateway/internal/extagent"
)

// 外部 agent（设计 §16.4）：主人在网页上登记（data/external-agents.json），在工作目录上显式开启哪些能用。

// openExternalAgents 读入登记。打不开就保留现场，外部 agent 不可用，网关其余照旧。
func (a *App) openExternalAgents() {
	s, err := extagent.Open(filepath.Join(a.store.Dir(), "external-agents.json"))
	if err != nil {
		a.log.Error("external-agents.json 打不开，保留现场", "err", err)
		return
	}
	a.extAgents = s
}

// ExternalAgents 列出登记。
func (a *App) ExternalAgents() []extagent.Agent {
	if a.extAgents == nil {
		return []extagent.Agent{}
	}
	return a.extAgents.List()
}

// SaveExternalAgent 登记或替换一个外部 agent（按名字）。
func (a *App) SaveExternalAgent(ag extagent.Agent) error {
	if a.extAgents == nil {
		return errors.New("外部 agent 的登记现在不可用")
	}
	return a.extAgents.Put(ag)
}

// DeleteExternalAgent 删掉一个登记，并在所有工作目录上关掉它。
func (a *App) DeleteExternalAgent(name string) error {
	if a.extAgents == nil {
		return errors.New("外部 agent 的登记现在不可用")
	}
	if err := a.extAgents.Delete(name); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := false
	for i := range a.wss {
		if j := slices.Index(a.wss[i].External, name); j >= 0 {
			a.wss[i].External = slices.Delete(a.wss[i].External, j, j+1)
			changed = true
		}
	}
	if changed {
		if err := a.saveWorkspaces(); err != nil {
			return err
		}
		a.publishWorkspaces()
	}
	return nil
}

// SetWorkspaceExternal 设这个工作目录上开启哪些外部 agent（只能是登记了的）。工单的派生工作目录不能设。
func (a *App) SetWorkspaceExternal(id string, names []string) error {
	var list []string
	for _, n := range names {
		if slices.Contains(list, n) {
			continue
		}
		if a.extAgents == nil {
			return errors.New("外部 agent 的登记现在不可用")
		}
		if _, ok := a.extAgents.Get(n); !ok {
			return errors.New("没有登记这个外部 agent：" + n)
		}
		list = append(list, n)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.findWorkspace(id)
	if w == nil || w.Ticket != "" {
		return errors.New("没有这个工作目录")
	}
	old := w.External
	w.External = list
	if err := a.saveWorkspaces(); err != nil {
		w.External = old
		return err
	}
	a.publishWorkspaces()
	return nil
}
