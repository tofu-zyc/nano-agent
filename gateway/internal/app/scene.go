package app

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"nano-agent/gateway/internal/rooms"
	"nano-agent/gateway/internal/scene"
)

// sceneInfo 是一个会话所在的场景。
type sceneInfo struct {
	Kind  scene.Kind
	Key   string      // room:<id>，外部群是会话 ID
	Room  *rooms.Room // 内部群才有
	Rules scene.Rules // 没设规则是零值
}

// sceneOf 返回会话所在的场景；私聊（以及这一步没有规则的会话）返回 false。调用方持有锁。
//   - 内部群：记录会话 room:<id> 和成员会话 @<agent>/room:<id>，规则存在群里；
//   - 外部群：设置里 Group 为真、又不属于内部群的会话，规则存在这个会话的设置里。
func (a *App) sceneOf(sid string) (sceneInfo, bool) {
	if room, ok := a.roomOfSession(sid); ok {
		kind := scene.Room
		if room.IsDM() {
			kind = scene.DMRoom
		}
		return sceneInfo{Kind: kind, Key: roomLogSID(room.ID), Room: &room, Rules: room.Rules}, true
	}
	st := a.store.Settings(sid)
	if !st.Group {
		return sceneInfo{}, false
	}
	info := sceneInfo{Kind: scene.External, Key: sid}
	if st.Scene != nil {
		info.Rules = *st.Scene
	}
	return info, true
}

// sceneModeFree 是这个场景现在允许按积极度主动插话：内部群默认点名，外部群默认自由。
func (s sceneInfo) free() bool {
	switch s.Rules.Mode {
	case scene.ModeFree:
		return true
	case scene.ModeMention, scene.ModeTurns, scene.ModeDirector:
		return false // 回合制的时钟按回合走，不按真实时间：groupTick 和 observe 都不作用在它们上
	}
	return s.Kind == scene.External
}

// sceneRoleOf 是 agent 在场景里的角色（没设是零值）。
func (s sceneInfo) roleOf(agentID string) scene.Role { return s.Rules.Roles[agentID] }

// applyScene 把场景规则叠在 agent 的群聊规则上：每一项单独取，场景里有值就用场景的。
// 积极度：场景角色 → agent 的群聊积极度 → low；工具型 agent 只有角色里明确设了才会主动插话。
func (a *App) applyScene(sid string, rule groupRule) groupRule {
	sc, ok := a.sceneOf(sid)
	if !ok {
		return rule
	}
	if e := sc.roleOf(a.agentOf(sid).ID).Eagerness; e != "" {
		rule.Eagerness, rule.PerHour = groupPerHour(e)
		rule.Decisions = max(2, rule.PerHour*3)
	}
	st := sc.Rules.Style
	if d := parseLimitDuration(st.Quiet); d > 0 {
		rule.Quiet = d
	}
	if d := parseLimitDuration(st.MinGap); d > 0 {
		rule.MinGap = d
	}
	if st.MaxLen > 0 {
		rule.MaxLen = st.MaxLen
	}
	if st.Parts > 0 && st.Parts <= 5 {
		rule.Parts = st.Parts
	}
	return rule
}

// sceneToolAllow 是群聊里工具名的白名单：场景 Tools → agent 的群聊工具 → 默认表；
// 场景 Tools 有值、agent 也设了群聊工具时取交集。最后一步（去掉这一轮拿不到的）由调用方的候选表完成。
func (a *App) sceneToolAllow(sid string) []string {
	allow := a.agentOf(sid).GroupTools
	if len(allow) == 0 {
		allow = sceneToolNames()
	}
	if sc, ok := a.sceneOf(sid); ok && sc.Rules.Tools != nil {
		if len(a.agentOf(sid).GroupTools) == 0 {
			return slices.Clone(sc.Rules.Tools)
		}
		var both []string
		for _, n := range sc.Rules.Tools {
			if slices.Contains(allow, n) {
				both = append(both, n)
			}
		}
		return both
	}
	return allow
}

// sceneText 是放进成员系统前缀的场景内容：[场景] 说明，[你的角色] 提示词；没有就空。
// 同一个场景里保持不变（只是改规则时让这个场景成员的前缀缓存失效一次）。
func (a *App) sceneText(sid string) string {
	sc, ok := a.sceneOf(sid)
	if !ok {
		return ""
	}
	var parts []string
	if b := strings.TrimSpace(sc.Rules.Brief); b != "" {
		parts = append(parts, "[场景] "+b)
	}
	if p := strings.TrimSpace(sc.roleOf(a.agentOf(sid).ID).Prompt); p != "" {
		parts = append(parts, "[你的角色] "+p)
	}
	return strings.Join(parts, "\n\n")
}

// sceneGuard 是内部群生效的防刷屏阈值。
func (s sceneInfo) guard() (run, pingPong int) { return s.Rules.Guard.EffectiveGuard() }

// ScenePatch 是对场景规则的修改，字段都是可选的：没给的不动。
// 房间的接口把它平铺在 RoomPatch 里；tools 要区分“没给”“null（沿用默认）”和 []（不给工具），所以用原始 JSON。
type ScenePatch struct {
	Mode        *string                `json:"mode"`
	Observe     *bool                  `json:"observe"`
	Brief       *string                `json:"brief"`
	Roles       *map[string]scene.Role `json:"roles"`
	Tools       json.RawMessage        `json:"tools"`
	Style       *scene.Style           `json:"style"`
	Guard       *scene.Guard           `json:"guard"`
	Budget      *int                   `json:"budget"`
	BudgetTotal *int                   `json:"budget_total"`
	Order       *[]string              `json:"order"`
	Rounds      *int                   `json:"rounds"`
	Channels    *[]scene.Channel       `json:"channels"`
	Reset       bool                   `json:"reset"` // 把这一场累计的用量清零（含轮数）
}

// apply 返回改过的规则（不改传入的）。校验由调用方按场景种类做。
func (p ScenePatch) apply(r scene.Rules) (scene.Rules, error) {
	r = r.Clone()
	if p.Mode != nil {
		r.Mode = *p.Mode
	}
	if p.Observe != nil {
		r.Observe = *p.Observe
	}
	if p.Brief != nil {
		r.Brief = *p.Brief
	}
	if p.Roles != nil {
		r.Roles = *p.Roles
		if len(r.Roles) == 0 {
			r.Roles = nil
		}
	}
	if len(p.Tools) > 0 {
		if string(p.Tools) == "null" {
			r.Tools = nil
		} else {
			var tools []string
			if err := json.Unmarshal(p.Tools, &tools); err != nil {
				return r, errors.New("tools 要是字符串数组")
			}
			if tools == nil {
				tools = []string{}
			}
			r.Tools = tools
		}
	}
	if p.Style != nil {
		r.Style = *p.Style
	}
	if p.Guard != nil {
		r.Guard = *p.Guard
	}
	if p.Budget != nil {
		r.Budget = *p.Budget
	}
	if p.BudgetTotal != nil {
		r.BudgetTotal = *p.BudgetTotal
	}
	if p.Order != nil {
		r.Order = *p.Order
		if len(r.Order) == 0 {
			r.Order = nil
		}
	}
	if p.Rounds != nil {
		r.Rounds = *p.Rounds
	}
	if p.Channels != nil {
		r.Channels = *p.Channels
		if len(r.Channels) == 0 {
			r.Channels = nil
		}
	}
	return r, nil
}
