// Package proto 定义网关与内核之间的消息（一行一条 JSON），完整说明见 docs/protocol.md。
package proto

import "encoding/json"

// Version 是当前协议版本，内核在 hello 里带上。
const Version = 11

// MinVersion 是网关还愿意连的最老协议：比 Version 旧、但不比它旧的内核先放进来，
// 随后靠自动更新换成新的。自我更新是 v9 才有的，再老的放进来也推不了新版本，直接拒绝。
const MinVersion = 9

// 权限模式，随 input 传给内核。
const (
	ModeNone     = "none"     // 不能用工具
	ModeReadonly = "readonly" // 只能用只读工具（Read、Grep）
	ModeAsk      = "ask"      // 只读工具直接执行，其余每次先审批
	ModeAuto     = "auto"     // 全部直接执行，都在工作目录的沙箱里
	// ModeFull 是放行：内核照样按 auto 跑（沙箱和审批规则不变），要审批的调用由网关当场自动批准。
	// 只在网关里用，不发给内核。
	ModeFull = "full"
)

// 消息类型。
const (
	// 内核 → 网关
	THello        = "hello"
	TModel        = "model"
	TResult       = "result"
	TApproveReq   = "approve_req"
	TNotice       = "notice"
	TDone         = "done"
	TPing         = "ping"
	TLsResult     = "ls_result"
	TExec         = "exec"          // 请网关执行一次工具：工作目录不在这个内核的机器上，或者 Skill
	TExecCancel   = "exec_cancel"   // 工具超时或这一轮被停止：请网关停下
	TRunResult    = "run_result"    // run / skill_put 的结果
	TTools        = "tools"         // 旧内核：技能变化后更新工具定义（现在技能在网关上）
	TUpdateResult = "update_result" // update 的结果；成功后内核随即用新版本重启

	// 网关 → 内核
	TWelcome    = "welcome"
	TError      = "error"
	TInput      = "input"
	TTurn       = "turn"
	TApprove    = "approve"
	TCancel     = "cancel"
	TPong       = "pong"
	TLs         = "ls"          // 列目录，网页上选工作目录用
	TExecResult = "exec_result" // exec 的结果
	TRun        = "run"         // 请内核在它的机器上执行一次工具（工作目录在那台机器上，这一轮由别的内核跑）
	TRunCancel  = "run_cancel"  // 停下 run
	TSkillPut   = "skill_put"   // 把技能文件放到内核机器的独立技能缓存，run_result.content 返回实际目录
	// 协议 v10：MCP 服务的 stdio 透传（把 MCP 进程放到别的电脑上跑）
	TPipeOpen   = "pipe_open"   // 网关 → 内核：起一个子进程
	TPipeWrite  = "pipe_write"  // 网关 → 内核：往它的 stdin 写一行
	TPipeClose  = "pipe_close"  // 网关 → 内核：关掉（先关 stdin，不退就杀）
	TPipeData   = "pipe_data"   // 内核 → 网关：stdout 的一行
	TPipeClosed = "pipe_closed" // 内核 → 网关：进程退出了

	// 协议 v11：并行、排队与完整调用身份
	TToolQueued  = "tool_queued"  // 内核 → 网关：这个调用在等执行槽或等资源
	TToolStarted = "tool_started" // 内核 → 网关：这个调用真的开始执行了（时限从这时算）
	TExecStarted = "exec_started" // 网关 → 内核：交给网关的调用真的开始了
	TInputPending = "input_pending" // 网关 → 内核：当前 run 收到了插话（只记 revision，不改调度）
	TInputReady   = "input_ready"   // 内核 → 网关：到了安全边界，可以写插话了
	TInputCommit  = "input_commit"  // 网关 → 内核：插话已经写进历史，可以请求下一次模型
	TModelYield   = "model_yield"   // 网关 → 内核：这次生成让位给插话，不算错误、不算停止

	TPairing = "pairing" // 收到了配对请求，等管理员确认（Code 是确认码）
	TPaired  = "paired"  // 管理员同意了：Token 是这台电脑专用的口令，内核存好后重新连接
	TUpdate  = "update"  // 新版本的程序（Files：path 是文件名，data 是 base64），内核换掉自己后重启
)

// error 的 reason：内核据此决定怎么办
const (
	ErrAuth   = "auth"   // 口令不对（配对得到的口令失效了就重新配对）
	ErrDenied = "denied" // 管理员拒绝了配对，内核退出
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
	V     int    `json:"v,omitempty"`
	Token string `json:"token,omitempty"`
	Pair  bool   `json:"pair,omitempty"` // hello：没有口令，请求配对
	Code  string `json:"code,omitempty"` // pairing：确认码，网页上核对
	Node  string `json:"node,omitempty"`
	OS    string `json:"os,omitempty"`   // hello：内核所在的系统 linux / windows / macos
	Arch  string `json:"arch,omitempty"` // hello：内核所在的 CPU 架构
	// hello：内核的版本；update / update_result：要换成的版本
	Version string `json:"version,omitempty"`
	// hello：上次自动更新失败、已经退回的版本，网关别再自动推它
	UpdateFailed string      `json:"update_failed,omitempty"`
	Tools        []ToolSpec  `json:"tools,omitempty"`
	Skills       []SkillSpec `json:"skills,omitempty"`
	Sandbox      *Sandbox    `json:"sandbox,omitempty"`

	SID    string `json:"sid,omitempty"`
	From   string `json:"from,omitempty"`
	Reason string `json:"reason,omitempty"`
	Calls  []Call `json:"calls,omitempty"`

	// 完整调用身份（协议 v11）：一次 run 内 sid + run_id + turn_id + call_id 唯一确定一个工具调用。
	// 迟到、重复、跨 run 的结果靠它们配对，不再只看模型给的 call id。
	RunID     string `json:"run_id,omitempty"`
	TurnID    int    `json:"turn_id,omitempty"`
	RequestID string `json:"request_id,omitempty"` // 控制消息的请求编号（回应时原样带回）
	// Revision 是插话的消费游标（input_pending / input_ready / input_commit 用）
	Revision int `json:"revision,omitempty"`
	// 本机能力与限额：内核在 hello 里报，网关按机器协商
	Features map[string]json.RawMessage `json:"features,omitempty"`
	// MaxParallelTools 是网关希望这一轮同时跑几个工具（0 用内核配置）
	MaxParallelTools int `json:"max_parallel_tools,omitempty"`
	// SandboxMode 是这一轮请求的沙箱模式（auto / on / off）；执行节点的本机要求优先
	SandboxMode string `json:"sandbox_mode,omitempty"`

	// input：这一轮的权限模式和挂载的工作目录；elevate 表示这个身份能不能提权。
	// 最多请求模型几次、总时限、审批时限：大于 0 是上限，小于 0 表示不限（不带就用内核自己的配置）；
	// max_output（和 exec 共用）是工具结果最多保留的字节数
	MaxSteps          int             `json:"max_steps,omitempty"`
	RunTimeoutMs      int             `json:"run_timeout_ms,omitempty"`
	ApprovalTimeoutMs int             `json:"approval_timeout_ms,omitempty"`
	Mode              string          `json:"mode,omitempty"`
	Elevate           bool            `json:"elevate,omitempty"`
	Workspaces        []WorkspaceSpec `json:"workspaces,omitempty"`

	// exec：在哪个工作目录上执行（Skill 没有）、工具的时限和输出上限
	Workspace string `json:"workspace,omitempty"`
	Elevated  bool   `json:"elevated,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	MaxOutput int    `json:"max_output,omitempty"`
	// run：交给执行器的完整请求；skill_put：技能名和文件
	Req   json.RawMessage `json:"req,omitempty"`
	Skill string          `json:"skill,omitempty"`
	Files []SkillFile     `json:"files,omitempty"`

	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	OK      *bool           `json:"ok,omitempty"`
	Content string          `json:"content,omitempty"`
	Status  string          `json:"status,omitempty"`
	Detail  string          `json:"detail,omitempty"`
	Text    string          `json:"text,omitempty"`

	// pipe（协议 v10）：MCP 服务的 stdio 透传
	Pipe string            `json:"pipe,omitempty"` // 这条管道的编号
	Argv []string          `json:"argv,omitempty"` // pipe_open：要跑的命令和参数
	Env  map[string]string `json:"env,omitempty"`  // pipe_open：环境变量
	Cwd  string            `json:"cwd,omitempty"`  // pipe_open：工作目录
	Data string            `json:"data,omitempty"` // pipe_write / pipe_data：一行内容
	Exit int               `json:"exit,omitempty"` // pipe_closed：退出码

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

// WorkspaceSpec 是 input 里挂载的一个工作目录。Name 是给模型看的名字（机器:路径），
// 工具参数里的 workspace 用它。Local 表示在跑这一轮的内核的机器上，内核自己执行，
// 否则交给网关（Landlock 是那台机器的沙箱版本）。
type WorkspaceSpec struct {
	Name     string   `json:"name"`
	Dirs     []string `json:"dirs"`
	RO       []string `json:"ro"`
	RW       []string `json:"rw"`
	Home     bool     `json:"home,omitempty"` // 没有工作目录时的 HOME 模式：dirs 为空，相对路径和 Shell 用 HOME
	Local    bool     `json:"local,omitempty"`
	Landlock int      `json:"landlock,omitempty"`
	OS       string   `json:"os,omitempty"` // 这台机器上的路径属于哪个系统：linux / macos / windows
}

// SkillSpec 是技能简介，正文在调用 Skill 时才加载。
type SkillSpec struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SkillFile 是技能目录里的一个文件，Data 是 base64。
type SkillFile struct {
	Path string `json:"path"`
	Data string `json:"data"`
	Exec bool   `json:"exec,omitempty"`
}

// Sandbox 是内核报告的沙箱能力。Landlock 为 0 表示没有沙箱，自动模式下的 Shell 也要审批。
// Mode 是请求的模式（auto / on / off），Effective 是实际生效的程度（full / partial / none）。
type Sandbox struct {
	Landlock  int    `json:"landlock"`
	Mode      string `json:"mode,omitempty"`
	Effective string `json:"effective,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Feature 取能力集合里的一个数（没有时返回 0）。
func (m *Msg) Feature(name string) int {
	raw, ok := m.Features[name]
	if !ok {
		return 0
	}
	var n int
	if json.Unmarshal(raw, &n) != nil {
		return 0
	}
	return n
}

// FeatureString 取能力集合里的一个字符串（没有时返回空）。
func (m *Msg) FeatureString(name string) string {
	raw, ok := m.Features[name]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
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
