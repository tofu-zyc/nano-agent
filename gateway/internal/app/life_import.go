package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"nano-agent/gateway/internal/autonomy"
	"nano-agent/gateway/internal/experience"
)

// 旧版 data/life.json 的内容并进 data/autonomy.json（设计 §15.1 R2）。
//
// 只导入一次：成功后 life.json 改名为 life.json.imported，之后不再读写。
// 失败时（读不动、经历写不进去、autonomy.json 写不进去）两个文件都不动，
// 健康条目 autonomy 变成 error，自主活动不启动，修好后重启再试。
//
// 导入的内容：下次醒来（并进 NextCheck）、今天醒了几次和找了几次人、今天用了多少 token（进账本）、
// 找人记录（Awaiting）、回访（FollowUps）；还没导入经历的旧近况（Recent）先导进经历。

// legacyLife 是 life.json 里一个 agent 的旧状态。
type legacyLife struct {
	ExperienceImported bool                         `json:"experience_imported,omitempty"`
	Next               int64                        `json:"next,omitempty"`
	Day                string                       `json:"day,omitempty"`
	Wakes              int                          `json:"wakes,omitempty"`
	Reaches            int                          `json:"reaches,omitempty"`
	Tokens             int                          `json:"tokens,omitempty"`
	Recent             []lifeNote                   `json:"recent,omitempty"`
	Awaiting           map[string]int64             `json:"awaiting,omitempty"`
	FollowUps          map[string]autonomy.FollowUp `json:"follow_ups,omitempty"`
}

// importLifeOnLoad 在读完 autonomy.json 之后并入 life.json。成功（或不需要导入）返回 true；
// 失败时记到 mindsErr 和健康条目，返回 false。调用方持有锁。
func (a *App) importLifeOnLoad() bool {
	if err := a.importLife(); err != nil {
		a.setMindsErr(fmt.Errorf("旧的 life.json 导入失败，两个文件都没动（修好后重启再试）：%w", err))
		a.log.Error(a.mindsErr.Error())
		return false
	}
	return true
}

func (a *App) importLife() error {
	if a.minds == nil {
		a.minds = map[string]*autonomy.State{}
	}
	var legacy map[string]*legacyLife
	b, err := os.ReadFile(a.lifeFile())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &legacy); err != nil {
			return fmt.Errorf("life.json 解析失败：%w", err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	now := a.now().In(a.loc)
	day := autonomy.LedgerDay(now, a.loc)
	staged := map[string]*autonomy.State{} // 改在副本上做，全部成功才换进去
	for id, st := range a.minds {
		clone, err := cloneMind(st)
		if err != nil {
			return err
		}
		staged[id] = clone
	}
	for id, ls := range legacy {
		if ls == nil {
			continue
		}
		st := staged[id]
		if st == nil {
			st = autonomy.New()
			staged[id] = st
		}
		if st.Migrated&autonomy.MigratedLifeJSON != 0 {
			continue // 这个 agent 已经导入过（上次改名没成功）：不重复
		}
		if err := a.importLifeAgent(id, st, ls, now, day); err != nil {
			return fmt.Errorf("导入 %s：%w", id, err)
		}
	}
	changed := false
	for _, st := range staged {
		if st.Migrated&autonomy.MigratedLifeJSON == 0 || st.SchemaVersion < autonomy.SchemaVersion {
			// 以前预算里记的用量（含自主分支）比账本多的部分补进账本，今天的数字不变
			if st.Budget.Day == day {
				if have := st.TokensOn(day); st.Budget.Tokens > have {
					st.AddLedgerTokens(autonomy.LedgerOther, st.Budget.Tokens-have, now, a.loc)
				}
			}
			st.MarkLifeImported()
			changed = true
		}
	}
	if legacy == nil && !changed {
		return nil
	}
	// 先把新的 autonomy.json 写下来，再换进内存，最后才给 life.json 改名
	if err := a.writeMinds(staged); err != nil {
		return fmt.Errorf("写 autonomy.json 失败：%w", err)
	}
	a.minds = staged
	if legacy != nil {
		to := a.lifeFile() + ".imported"
		if _, err := os.Stat(to); err == nil {
			to = fmt.Sprintf("%s.%d", to, time.Now().Unix()) // 已经有一份：不覆盖
		}
		if err := os.Rename(a.lifeFile(), to); err != nil {
			// autonomy.json 已经记下导入过了，下次启动不会重复导入；只是文件还没改名
			a.log.Warn("life.json 已导入但改名失败", "err", err)
		} else {
			a.log.Info("旧的 life.json 已并进 autonomy.json", "agents", len(legacy), "to", to)
		}
	}
	return nil
}

// importLifeAgent 把一个 agent 的旧状态并进它的心智状态副本。
func (a *App) importLifeAgent(id string, st *autonomy.State, ls *legacyLife, now time.Time, today string) error {
	// 还没导入经历的旧近况：先导进经历（稳定来源键保证部分导入后重来不重复）
	if !ls.ExperienceImported && len(ls.Recent) > 0 {
		s, err := experience.Open(a.store.Dir(), id)
		if err != nil {
			return fmt.Errorf("经历记录打不开：%w", err)
		}
		for i, n := range ls.Recent {
			e := experience.Entry{Kind: "life", Where: "旧版内心活动", Visible: "admin", Said: n.Text, Status: "ok",
				Sources: []string{fmt.Sprintf("legacy-recent:%d:%d", n.At, i)}}
			if err := s.Append(e, time.Unix(n.At, 0)); err != nil {
				return fmt.Errorf("旧近况导入失败：%w", err)
			}
		}
	}
	if st.NextCheck == 0 && ls.Next > 0 {
		st.SetNextCheck(time.Unix(ls.Next, 0))
	}
	for sid, at := range ls.Awaiting {
		if _, ok := st.Awaiting[sid]; !ok {
			st.AwaitingMap()[sid] = at
		}
	}
	for sid, f := range ls.FollowUps {
		if _, ok := st.FollowUps[sid]; !ok {
			st.FollowUpMap()[sid] = f
		}
	}
	// 今天的次数和用量进账本：每一项取旧的几个数里最大的那个，网页和状态显示的数字不变
	if ls.Day == today {
		st.RaiseLedgerStarts(autonomy.LedgerExploration, ls.Wakes, now, a.loc)
		st.RaiseLedgerStarts(autonomy.LedgerReach, ls.Reaches, now, a.loc)
		if have := st.TokensOn(today); ls.Tokens > have {
			st.AddLedgerTokens(autonomy.LedgerOther, ls.Tokens-have, now, a.loc)
		}
	}
	return nil
}

// cloneMind 复制一份心智状态（只在启动导入时用，那时还没有不落盘的运行时字段）。
func cloneMind(st *autonomy.State) (*autonomy.State, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	out := autonomy.New()
	if err := json.Unmarshal(b, out); err != nil {
		return nil, err
	}
	return out, nil
}
