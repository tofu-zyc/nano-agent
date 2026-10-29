// Package schedule 保存定时任务（data/schedules.json）、计算下次运行时间；执行和加锁由 app 负责。
package schedule

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/robfig/cron/v3"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Result struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Summary string    `json:"summary"`
}
type Task struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	When        string   `json:"when"`
	Agent       string   `json:"agent"`
	Prompt      string   `json:"prompt"`
	Deliver     []string `json:"deliver"`
	Workspaces  []string `json:"workspaces"`
	Mode        string   `json:"mode"`
	Enabled     bool     `json:"enabled"`
	CreatedBy   string   `json:"created_by"`
	CreatorMode string   `json:"creator_mode"`
	// Source 是创建任务时所在的会话（安静分支派生的任务靠它记录来源）；
	// Silent 是派生出来的有效投递策略：为 true 时结果只留任务页，不往任何聊天推。
	Source  string    `json:"source,omitempty"`
	Silent  bool      `json:"silent,omitempty"`
	CatchUp bool      `json:"catch_up"`
	Timeout string    `json:"timeout,omitempty"`
	Next    time.Time `json:"next"`
	Last    *Result   `json:"last,omitempty"`
	Results []Result  `json:"results"`
	Running bool      `json:"running"`
}

func NewID() string { b := make([]byte, 6); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func Timeout(t Task) (time.Duration, error) {
	if t.Timeout == "" {
		return 30 * time.Minute, nil
	}
	d, err := time.ParseDuration(t.Timeout)
	if err != nil || d <= 0 || d/time.Millisecond > 2147483647 {
		return 0, errors.New("timeout 需要正时长（毫秒数不能超过 32 位整数）")
	}
	return d, nil
}
func Next(spec string, after time.Time, loc *time.Location) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(spec, "every ")))
		if err != nil || d < time.Second {
			return time.Time{}, errors.New("every 后写至少 1s 的时长")
		}
		return after.Add(d), nil
	}
	if strings.HasPrefix(spec, "at ") {
		text := strings.TrimSpace(strings.TrimPrefix(spec, "at "))
		at, err := time.ParseInLocation("2006-01-02 15:04", text, loc)
		if err != nil || at.In(loc).Format("2006-01-02 15:04") != text {
			return time.Time{}, errors.New("at 后写当地时间 YYYY-MM-DD HH:mm，不能是夏令时跳过的时间")
		}
		if !at.After(after) {
			return time.Time{}, nil
		}
		return at, nil
	}
	if len(strings.Fields(spec)) != 5 {
		return time.Time{}, errors.New("when 需要 cron 五段式、every 30m 或 at YYYY-MM-DD HH:mm")
	}
	p := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	c, err := p.Parse(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron 不正确：%w", err)
	}
	next := c.Next(after.In(loc))
	if next.IsZero() {
		return next, errors.New("cron 在未来五年内没有运行时间")
	}
	return next, nil
}

// Advance 算出下一个还没到的运行时间：every 直接跳过错过的次数，cron 从现在往后找
func Advance(t Task, now time.Time, loc *time.Location) (time.Time, error) {
	if strings.HasPrefix(strings.TrimSpace(t.When), "every ") && !t.Next.IsZero() {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t.When), "every ")))
		if err != nil || d < time.Second {
			return time.Time{}, errors.New("every 时长无效")
		}
		if t.Next.After(now) {
			return t.Next, nil
		}
		return t.Next.Add((now.Sub(t.Next)/d + 1) * d), nil
	}
	return Next(t.When, now, loc)
}

// LatestDue 找最近一次错过的运行时间（停机期间错过多次也只补最近一次）
func LatestDue(t Task, now time.Time, loc *time.Location) time.Time {
	spec := strings.TrimSpace(t.When)
	if strings.HasPrefix(spec, "every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(spec, "every ")))
		if err == nil && d >= time.Second && !t.Next.IsZero() && !t.Next.After(now) {
			return t.Next.Add((now.Sub(t.Next) / d) * d)
		}
		return t.Next
	}
	if strings.HasPrefix(spec, "at ") {
		return t.Next
	}
	cursor := now.Add(-time.Hour).Add(-time.Nanosecond)
	if t.Next.After(cursor) {
		cursor = t.Next.Add(-time.Nanosecond)
	}
	last := t.Next
	for i := 0; i < 62; i++ {
		next, err := Next(spec, cursor, loc)
		if err != nil || next.IsZero() || next.After(now) {
			break
		}
		last = next
		cursor = next
	}
	return last
}

func Record(t *Task, r Result) {
	t.Last = &r
	t.Results = append(t.Results, r)
	if len(t.Results) > 10 {
		t.Results = append([]Result(nil), t.Results[len(t.Results)-10:]...)
	}
}

type Store struct {
	path  string
	tasks []Task
}

func Open(path string, now time.Time) (*Store, error) {
	st := &Store{path: path, tasks: []Task{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &st.tasks); err != nil {
		return nil, fmt.Errorf("读取 schedules.json：%w", err)
	}
	seen := map[string]bool{}
	dirty := false
	for i := range st.tasks {
		t := &st.tasks[i]
		if t.ID == "" || seen[t.ID] {
			return nil, errors.New("schedules.json 任务编号为空或重复")
		}
		seen[t.ID] = true
		if t.Running {
			t.Running = false
			Record(t, Result{At: now, Summary: "网关重启，上一轮已中断；不重复执行"})
			dirty = true
		}
	}
	if dirty {
		if err = st.Save(st.tasks); err != nil {
			return nil, err
		}
	}
	return st, nil
}
func (s *Store) List() []Task {
	b, _ := json.Marshal(s.tasks)
	var tasks []Task
	_ = json.Unmarshal(b, &tasks)
	if tasks == nil {
		return []Task{}
	}
	return tasks
}
func (s *Store) Save(tasks []Task) error {
	b, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, s.path); err != nil {
		return err
	}
	_ = json.Unmarshal(b, &s.tasks)
	return nil
}
