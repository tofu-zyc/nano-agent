package app

import (
	"errors"
	"path/filepath"
	"slices"

	"nano-agent/gateway/internal/extagent"
)

// 外部 agent（设计 §16.4）：主人在网页上登记（data/external-agents.json），在工作目录上显式开启哪些能用。

// openExternalAgents 读入登记。打不开就保留现场，外部 agent 不可用，网关其余照旧；
// 单项没通过校验的不能用，记一条警告。
func (a *App) openExternalAgents() {
	s, err := extagent.Open(filepath.Join(a.store.Dir(), "external-agents.json"))
	if err != nil {
		a.log.Error("external-agents.json 打不开，保留现场", "err", err)
		return
	}
	for _, p := range s.Problems() {
		a.log.Warn("external-agents.json 里这一项不能用", "name", p.Name, "err", p.Err)
	}
	a.extAgents = s
}

// ExternalAgents 列出登记。
func (a *App) ExternalAgents() []extagent.Agent {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.extAgents == nil {
		return []extagent.Agent{}
	}
	return a.extAgents.List()
}

// SaveExternalAgent 登记或替换一个外部 agent（按名字），返回因此关掉的工作目录（ID）。
// 改了命令、目录或环境，所有工作目录上都关掉它：开启时确认的是那时候登记的目录。新登记的名字也一样，
// 先清掉工作目录上残留的同名开关。先关再改登记：登记没写成，开关也已经关了。
func (a *App) SaveExternalAgent(ag extagent.Agent) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.extAgents == nil {
		return nil, errors.New("外部 agent 的登记现在不可用")
	}
	if err := ag.Validate(); err != nil {
		return nil, err
	}
	var off []string
	if prev, ok := a.extAgents.Get(ag.Name); !ok || !extagent.SameGrant(prev, ag) {
		var err error
		if off, err = a.disableExternalLocked(ag.Name); err != nil {
			return nil, err
		}
	}
	if err := a.extAgents.Put(ag); err != nil {
		return off, err
	}
	return off, nil
}

// DeleteExternalAgent 删掉一个登记，先在所有工作目录上关掉它。
func (a *App) DeleteExternalAgent(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.extAgents == nil {
		return errors.New("外部 agent 的登记现在不可用")
	}
	if !a.extAgents.Has(name) {
		return errors.New("没有这个外部 agent")
	}
	if _, err := a.disableExternalLocked(name); err != nil {
		return err
	}
	return a.extAgents.Delete(name)
}

// disableExternalLocked 在所有工作目录上关掉 name，返回关掉的工作目录。调用方持有锁。
func (a *App) disableExternalLocked(name string) ([]string, error) {
	var off []string
	old := make(map[int][]string)
	for i := range a.wss {
		if j := slices.Index(a.wss[i].External, name); j >= 0 {
			old[i] = a.wss[i].External
			a.wss[i].External = slices.Delete(slices.Clone(a.wss[i].External), j, j+1)
			off = append(off, a.wss[i].ID)
		}
	}
	if len(off) == 0 {
		return nil, nil
	}
	if err := a.saveWorkspaces(); err != nil {
		for i, ext := range old {
			a.wss[i].External = ext
		}
		return nil, err
	}
	a.publishWorkspaces()
	return off, nil
}

// SetWorkspaceExternal 设这个工作目录上开启哪些外部 agent（只能是登记了的）。工单的派生工作目录不能设。
func (a *App) SetWorkspaceExternal(id string, names []string) error {
	return a.PatchWorkspace(id, nil, &names)
}

// PatchWorkspace 改工作目录的执行隔离和外部 agent 开关（nil 的不改）：两项都校验完再动手，一起写盘。
func (a *App) PatchWorkspace(id string, sandboxMode *string, external *[]string) error {
	if sandboxMode != nil {
		if err := checkSandboxMode(*sandboxMode); err != nil {
			return err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.findWorkspace(id)
	if w == nil {
		return errors.New("没有这个工作目录")
	}
	var list []string
	if external != nil {
		if w.Ticket != "" {
			return errors.New("工单的工作目录跟着仓库的工作目录开关外部 agent，不能单独设")
		}
		for _, n := range *external {
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
	}
	oldMode, oldExt := w.SandboxMode, w.External
	if sandboxMode != nil {
		w.SandboxMode = *sandboxMode
	}
	if external != nil {
		w.External = list
	}
	if err := a.saveWorkspaces(); err != nil {
		w.SandboxMode, w.External = oldMode, oldExt
		return err
	}
	a.publishWorkspaces()
	return nil
}
