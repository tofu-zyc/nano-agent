package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"nano-agent/gateway/internal/eventlog"
)

type Endpoint struct {
	Channel     string `json:"channel"`
	Chat        string `json:"chat"`
	Fingerprint string `json:"fingerprint,omitempty"`
}
type Scope struct {
	Tools      []string   `json:"tools,omitempty"`
	SID        string     `json:"source_sid"`
	Mode       string     `json:"mode"`
	Workspaces []string   `json:"workspaces"`
	Endpoints  []Endpoint `json:"endpoints,omitempty"`
}
type Filter struct {
	JobID     string            `json:"job_id,omitempty"`
	Workspace string            `json:"workspace_id,omitempty"`
	Status    []string          `json:"status,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	Glob      string            `json:"path_glob,omitempty"`
}
type Handler struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	Command      string `json:"command,omitempty"`
	Script       string `json:"script,omitempty"`
	Digest       string `json:"digest,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	BranchSID    string `json:"branch_sid,omitempty"`
	TimeoutMS    int    `json:"timeout_ms,omitempty"`
	RetrySafe    bool   `json:"retry_safe,omitempty"`
	TokenBudget  int    `json:"token_budget,omitempty"`
}
type Watch struct {
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
}
type Spec struct {
	Name       string   `json:"name"`
	On         []string `json:"on"`
	Match      Filter   `json:"match,omitempty"`
	Handler    Handler  `json:"handler"`
	Scope      Scope    `json:"scope"`
	Deliver    string   `json:"deliver,omitempty"`
	DebounceMS int      `json:"debounce_ms,omitempty"`
	CooldownMS int      `json:"cooldown_ms,omitempty"`
	Retries    int      `json:"retries,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	OnError    string   `json:"on_error,omitempty"`
	DeadlineAt int64    `json:"deadline_at,omitempty"`
	Watch      *Watch   `json:"watch,omitempty"`
}

var EventNames = []string{"run.started", "run.finished", "approval.waiting", "tool.before", "tool.after", "tool.failed",
	"job.completed", "job.failed", "job.cancelled", "job.timed_out", "message.received", "device.online", "device.offline",
	"file.changed", "mind.threshold_crossed", "deadline.at"}

func Normalize(s Spec) (Spec, error) {
	if s.Retries == 0 {
		s.Retries = 2
	}
	if s.Handler.Type == "" {
		s.Handler.Type = "notify"
	}
	if s.Handler.Conversation == "" {
		s.Handler.Conversation = "temporary"
	}
	if s.Deliver == "" {
		s.Deliver = "creator"
	}
	if s.OnError == "" {
		s.OnError = "deny"
	}
	if s.Handler.TimeoutMS == 0 {
		s.Handler.TimeoutMS = 5000
		if s.Handler.Type == "agent" {
			s.Handler.TimeoutMS = 300000
		}
	}
	if slices.Contains(s.On, "deadline.at") && s.DeadlineAt <= 0 {
		return s, errors.New("deadline.at 需要正的 Unix 毫秒时间")
	}
	before := slices.Contains(s.On, "tool.before")
	if before && s.Handler.TimeoutMS > 2000 {
		s.Handler.TimeoutMS = 2000
	}
	if len(s.On) == 0 || len(s.On) > 16 || len(s.Name) > 120 || len(s.Scope.SID) > 256 || len(s.Scope.Tools) > 128 || len(s.Scope.Workspaces) > 16 || len(s.Scope.Endpoints) > 8 {
		return s, errors.New("Hook 事件、名称或来源范围超限")
	}
	for _, name := range s.On {
		if !slices.Contains(EventNames, name) {
			return s, fmt.Errorf("不支持的 Hook 事件 %q", name)
		}
	}
	if !slices.Contains([]string{"notify", "command", "agent"}, s.Handler.Type) ||
		!slices.Contains([]string{"temporary", "origin", "life", "branch"}, s.Handler.Conversation) ||
		!slices.Contains([]string{"creator", "origin", "none"}, s.Deliver) ||
		!slices.Contains([]string{"allow", "deny"}, s.OnError) ||
		!slices.Contains([]string{"none", "readonly", "ask", "auto", "full"}, s.Scope.Mode) {
		return s, errors.New("Hook 处理器、目标或权限模式无效")
	}
	if before && s.Handler.Type != "command" {
		return s, errors.New("tool.before 只允许短命令或声明式判断")
	}
	if len(s.Handler.Text) > 2000 || len(s.Handler.Command) > 2000 || len(s.Handler.Script) > 1024 || len(s.Handler.Prompt) > 2000 ||
		len(s.Match.Fields) > 16 || len(s.Match.Status) > 16 || len(s.Match.Glob) > 256 ||
		s.DebounceMS < 0 || s.DebounceMS > 60000 || s.CooldownMS < 0 || s.CooldownMS > 86400000 ||
		s.Retries < 0 || s.Retries > 7 || s.Handler.TimeoutMS < 1 || s.Handler.TimeoutMS > 600000 || s.Handler.TokenBudget < 0 {
		return s, errors.New("Hook 配置超过时限、正文、字段或重试上限")
	}
	if s.Handler.Type == "command" {
		if s.Handler.Command == "" && s.Handler.Script == "" {
			return s, errors.New("命令处理器需要 command 或已登记的 script")
		}
		if s.Handler.Command != "" && s.Handler.Script != "" {
			return s, errors.New("command 和 script 只能选择一个")
		}
		if s.Handler.TimeoutMS > 30000 {
			return s, errors.New("Hook 命令最长 30 秒；长计算请移交 Job")
		}
		if s.Handler.Script != "" && (s.Handler.Digest != "" && len(s.Handler.Digest) != 64) {
			return s, errors.New("脚本必须登记固定内容摘要")
		}
	}
	if s.Handler.Type == "agent" && s.Handler.Prompt == "" {
		return s, errors.New("agent 处理器需要明确任务说明")
	}
	if s.Match.Glob != "" {
		if _, err := path.Match(s.Match.Glob, ""); err != nil {
			return s, err
		}
	}
	for key, value := range s.Match.Fields {
		if len(key) > 64 || len(value) > 256 {
			return s, errors.New("Hook 字段比较超限")
		}
	}
	if s.Watch != nil && (s.Watch.Workspace == "" || s.Watch.Path == "" || len(s.Watch.Path) > 1024) {
		return s, errors.New("文件观察需要明确工作目录及路径")
	}
	raw, _ := json.Marshal(s)
	if len(raw) > 4096 {
		return s, errors.New("Hook 固定配置最多 4 KiB")
	}
	return s, nil
}
func Parse(record eventlog.RuleRecord) (Spec, error) {
	var s Spec
	if err := json.Unmarshal(record.Body, &s); err != nil {
		return s, err
	}
	return Normalize(s)
}
func Matches(s Spec, e eventlog.Event) bool {
	if !slices.Contains(s.On, e.Name) || s.Match.JobID != "" && s.Match.JobID != e.JobID {
		return false
	}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(e.Data, &data)
	scalar := func(key string) string {
		var value any
		if json.Unmarshal(data[key], &value) != nil {
			return ""
		}
		switch v := value.(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case bool:
			return strconv.FormatBool(v)
		}
		return ""
	}
	if s.Match.Workspace != "" && scalar("workspace_id") != s.Match.Workspace {
		return false
	}
	if len(s.Match.Status) > 0 && !slices.Contains(s.Match.Status, scalar("status")) {
		return false
	}
	for key, want := range s.Match.Fields {
		if scalar(key) != want {
			return false
		}
	}
	if s.Match.Glob != "" {
		matched, _ := path.Match(s.Match.Glob, strings.ReplaceAll(scalar("path"), "\\", "/"))
		if !matched {
			return false
		}
	}
	return true
}
func ObjectKey(e eventlog.Event) string {
	value := e.JobID
	if value == "" {
		var data map[string]json.RawMessage
		_ = json.Unmarshal(e.Data, &data)
		var file string
		_ = json.Unmarshal(data["path"], &file)
		value = e.SourceID + "\x00" + file
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func Template(text string, e eventlog.Event) string {
	values := map[string]string{"event_name": e.Name, "job_id": e.JobID, "agent_id": e.AgentID, "data_ref": e.DataRef, "source_id": e.SourceID}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(e.Data, &data)
	for _, key := range []string{"status", "path", "workspace_id"} {
		var v string
		if json.Unmarshal(data[key], &v) == nil {
			values[key] = v
		}
	}
	for key, value := range values {
		text = strings.ReplaceAll(text, "{{"+key+"}}", value)
	}
	if len(text) > 4000 {
		text = string([]rune(text)[:min(len([]rune(text)), 1000)])
	}
	return text
}
