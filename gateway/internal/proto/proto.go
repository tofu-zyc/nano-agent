// Package proto 定义网关与内核之间的消息（一行一条 JSON），完整说明见 docs/protocol.md。
package proto

import "encoding/json"

// Version 是当前协议版本，内核在 hello 里带上。
const Version = 3

// 权限模式，随 input 传给内核。
const (
	ModeNone     = "none"     // 不能用工具
	ModeReadonly = "readonly" // 只能用只读工具（Read、Grep）
	ModeAsk      = "ask"      // 只读工具直接执行，其余每次先审批
	ModeAuto     = "auto"     // 全部直接执行，都在工作目录的沙箱里
)

// 消息类型。
const (
	// 内核 → 网关
	THello      = "hello"
	TModel      = "model"
	TResult     = "result"
	TApproveReq = "approve_req"
	TNotice     = "notice"
	TDone       = "done"
	TPing       = "ping"
	TLsResult   = "ls_result"

	// 网关 → 内核
	TWelcome = "welcome"
	TError   = "error"
	TInput   = "input"
	TTurn    = "turn"
	TApprove = "approve"
	TCancel  = "cancel"
	TPong    = "pong"
	TLs      = "ls" // 列目录，网页上选工作目录用
)

// turn 的 reason。各家 API 的特殊情况由网关消化掉，内核只看到这四种。
const (
	ReasonFinal   = "final"
	ReasonToolUse = "tool_use"
	ReasonRefused = "refused"
	ReasonError   = "error"
)

// done 的 status。
const (
	StatusOK        = "ok"
	StatusBudget    = "budget"
	StatusTimeout   = "timeout"
	StatusCancelled = "cancelled"
	StatusRefused   = "refused"
	StatusError     = "error"
)

// Msg 是所有消息的并集，按 T 区分类型，没用到的字段省略。
type Msg struct {
	T string `json:"t"`

	// hello / welcome
	V       int        `json:"v,omitempty"`
	Token   string     `json:"token,omitempty"`
	Node    string     `json:"node,omitempty"`
	Tools   []ToolSpec `json:"tools,omitempty"`
	Sandbox *Sandbox   `json:"sandbox,omitempty"`

	SID    string `json:"sid,omitempty"`
	From   string `json:"from,omitempty"`
	Reason string `json:"reason,omitempty"`
	Calls  []Call `json:"calls,omitempty"`

	// input：这一轮的权限模式和工作目录
	Mode string   `json:"mode,omitempty"`
	Dirs []string `json:"dirs,omitempty"` // 工作目录，第一个是默认目录
	RO   []string `json:"ro,omitempty"`   // 额外的只读路径
	RW   []string `json:"rw,omitempty"`   // 额外的读写路径

	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Content string          `json:"content,omitempty"`
	Status  string          `json:"status,omitempty"`
	Detail  string          `json:"detail,omitempty"`
	Text    string          `json:"text,omitempty"`

	// ls / ls_result
	Path      string     `json:"path,omitempty"`
	Home      string     `json:"home,omitempty"`
	Blocked   bool       `json:"blocked,omitempty"` // 这个目录包含内核自己，不能当工作目录
	Entries   []DirEntry `json:"entries,omitempty"`
	Truncated bool       `json:"truncated,omitempty"`
}

// DirEntry 是 ls_result 里的一个子目录。
type DirEntry struct {
	Name    string `json:"name"`
	Blocked bool   `json:"blocked,omitempty"`
}

// ToolSpec 是内核报上来的工具描述，原样转成模型的工具定义。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Sandbox 是内核报告的沙箱能力。Landlock 为 0 表示没有沙箱，自动模式下的 Shell 也要审批。
type Sandbox struct {
	Landlock int `json:"landlock"`
}

// Call 是模型请求的一次工具调用。Args 是参数对象；模型给的参数不是合法 JSON 对象时，
// 是一个包含原始文本的 JSON 字符串，由内核判定为错误。
type Call struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// Bool 返回指向 b 的指针，用于 Msg.OK。
func Bool(b bool) *bool { return &b }
