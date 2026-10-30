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
	Decision     string `json:"decision,omitempty"`
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

// MaxScopedTools 是规则里冻结的工具名上限。网关在登记时把"来源会话当时能用的工具"
// 写进 scope.tools（只对会跑模型的 agent 处理器有意义），配置总长另有 4 KiB 上限。
const MaxScopedTools = 128

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
	// 每个上限单独说清是哪一个：合成一句话的时候没人能看出是哪一项挡住的。
	if len(s.On) == 0 {
		return s, errors.New("Hook 至少要登记一个事件（on）")
	}
	if len(s.On) > 16 {
		return s, fmt.Errorf("Hook 事件最多 16 个，收到 %d 个", len(s.On))
	}
	if len(s.Name) > 120 {
		return s, fmt.Errorf("Hook 名称最长 120 字节，收到 %d 字节", len(s.Name))
	}
	if len(s.Scope.SID) > 256 {
		return s, fmt.Errorf("Hook 来源会话 ID 最长 256 字节，收到 %d 字节", len(s.Scope.SID))
	}
	if len(s.Scope.Tools) > MaxScopedTools {
		return s, fmt.Errorf("Hook 冻结的工具最多 %d 个，收到 %d 个", MaxScopedTools, len(s.Scope.Tools))
	}
	if len(s.Scope.Workspaces) > 16 {
		return s, fmt.Errorf("Hook 来源工作目录最多 16 个，收到 %d 个", len(s.Scope.Workspaces))
	}
	if len(s.Scope.Endpoints) > 8 {
		return s, fmt.Errorf("Hook 投递窗口最多 8 个，收到 %d 个", len(s.Scope.Endpoints))
	}
	for _, name := range s.On {
		if !slices.Contains(EventNames, name) {
			return s, fmt.Errorf("不支持的 Hook 事件 %q", name)
		}
	}
	if !slices.Contains([]string{"notify", "command", "agent"}, s.Handler.Type) {
		return s, fmt.Errorf("handler.type 只能是 notify/command/agent，收到 %q", s.Handler.Type)
	}
	if !slices.Contains([]string{"temporary", "origin", "life", "branch"}, s.Handler.Conversation) {
		return s, fmt.Errorf("handler.conversation 只能是 temporary/origin/life/branch，收到 %q", s.Handler.Conversation)
	}
	if !slices.Contains([]string{"creator", "origin", "none"}, s.Deliver) {
		return s, fmt.Errorf("deliver 只能是 creator/origin/none，收到 %q", s.Deliver)
	}
	if !slices.Contains([]string{"allow", "deny"}, s.OnError) {
		return s, fmt.Errorf("on_error 只能是 allow/deny，收到 %q", s.OnError)
	}
	if !slices.Contains([]string{"none", "readonly", "ask", "auto", "full"}, s.Scope.Mode) {
		return s, fmt.Errorf("scope.mode 只能是 none/readonly/ask/auto/full，收到 %q", s.Scope.Mode)
	}
	if before && len(s.On) != 1 {
		return s, errors.New("前置和后置事件请登记为独立规则")
	}
	if s.Handler.Decision != "" && (!before || !slices.Contains([]string{"allow", "deny", "approval"}, s.Handler.Decision)) {
		return s, errors.New("声明式 decision 仅用于 tool.before：allow/deny/approval")
	}
	if before && s.Handler.Type != "command" {
		return s, errors.New("tool.before 只允许短命令或声明式判断")
	}
	switch {
	case len(s.Handler.Text) > 2000:
		return s, fmt.Errorf("handler.text 最长 2000 字节，收到 %d 字节", len(s.Handler.Text))
	case len(s.Handler.Prompt) > 2000:
		return s, fmt.Errorf("handler.prompt 最长 2000 字节，收到 %d 字节", len(s.Handler.Prompt))
	case len(s.Handler.Command) > 2000:
		return s, fmt.Errorf("handler.command 最长 2000 字节，收到 %d 字节", len(s.Handler.Command))
	case len(s.Handler.Script) > 1024:
		return s, fmt.Errorf("handler.script 最长 1024 字节，收到 %d 字节", len(s.Handler.Script))
	case len(s.Match.Fields) > 16:
		return s, fmt.Errorf("match.fields 最多 16 项，收到 %d 项", len(s.Match.Fields))
	case len(s.Match.Status) > 16:
		return s, fmt.Errorf("match.status 最多 16 项，收到 %d 项", len(s.Match.Status))
	case len(s.Match.Glob) > 256:
		return s, fmt.Errorf("match.path_glob 最长 256 字节，收到 %d 字节", len(s.Match.Glob))
	case s.DebounceMS < 0 || s.DebounceMS > 60000:
		return s, fmt.Errorf("debounce_ms 需在 0-60000 毫秒之间，收到 %d", s.DebounceMS)
	case s.CooldownMS < 0 || s.CooldownMS > 86400000:
		return s, fmt.Errorf("cooldown_ms 需在 0-86400000 毫秒之间，收到 %d", s.CooldownMS)
	case s.Retries < 0 || s.Retries > 7:
		return s, fmt.Errorf("retries 需在 0-7 之间，收到 %d", s.Retries)
	case s.Handler.TimeoutMS < 1 || s.Handler.TimeoutMS > 600000:
		return s, fmt.Errorf("handler.timeout_ms 需在 1-600000 毫秒之间，收到 %d", s.Handler.TimeoutMS)
	case s.Handler.TokenBudget < 0:
		return s, fmt.Errorf("handler.token_budget 不能是负数，收到 %d", s.Handler.TokenBudget)
	}
	if s.Handler.Type == "command" {
		if s.Handler.Command == "" && s.Handler.Script == "" && s.Handler.Decision == "" {
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
		return s, fmt.Errorf("Hook 固定配置最多 4 KiB，这条序列化后是 %d 字节（先缩小 scope 或 match）", len(raw))
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
