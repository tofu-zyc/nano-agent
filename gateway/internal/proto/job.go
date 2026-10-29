package proto

// JobSnapshot is the node's published ownership receipt. Frozen executor
// requests/command text are deliberately absent from this public projection.
type JobSnapshot struct {
	Version           int        `json:"version"`
	JobID             string     `json:"job_id"`
	NodeEpoch         string     `json:"node_epoch"`
	OwnerAgent        string     `json:"owner_agent"`
	CreatedBy         string     `json:"created_by"`
	OriginSID         string     `json:"origin_sid"`
	OriginRunID       string     `json:"origin_run_id,omitempty"`
	OriginCallID      string     `json:"origin_call_id,omitempty"`
	Origin            LeaseSpec  `json:"origin"`
	Lease             *LeaseSpec `json:"lease,omitempty"`
	WaitID            string     `json:"wait_id,omitempty"`
	WaitSID           string     `json:"wait_sid,omitempty"`
	WaitKind          int        `json:"wait_kind,omitempty"`
	Status            string     `json:"status"`
	Execution         string     `json:"execution"`
	Background        bool       `json:"background"`
	Revision          uint64     `json:"revision"`
	CreatedAt         int64      `json:"created_at"`
	StartedAt         int64      `json:"started_at,omitempty"`
	DeadlineAt        int64      `json:"deadline_at,omitempty"`
	EndedAt           int64      `json:"ended_at,omitempty"`
	TimeoutSeconds    int        `json:"timeout_seconds"`
	PID               int64      `json:"pid,omitempty"`
	SandboxEffective  string     `json:"sandbox_effective,omitempty"`
	OutputCursor      uint64     `json:"output_cursor"`
	OutputGap         bool       `json:"output_gap"`
	OutputTail        string     `json:"output_tail,omitempty"`
	LogError          string     `json:"log_error,omitempty"`
	Error             string     `json:"error,omitempty"`
	RecoveryReason    string     `json:"recovery_reason,omitempty"`
	LastEventSeq      uint64     `json:"last_event_seq,omitempty"`
	NotificationState string     `json:"notification_state,omitempty"`
	Result            *JobResult `json:"result,omitempty"`
}
type JobResult struct {
	OK               bool   `json:"ok"`
	Content          string `json:"content"`
	ExitCode         int64  `json:"exit_code"`
	Signal           int    `json:"signal"`
	TimedOut         bool   `json:"timed_out"`
	Cancelled        bool   `json:"cancelled"`
	SandboxEffective string `json:"sandbox_effective,omitempty"`
}
type JobOutput struct {
	Chunks          []JobChunk `json:"chunks"`
	NextCursor      uint64     `json:"next_cursor"`
	OutputCursor    uint64     `json:"output_cursor"`
	TruncatedBefore uint64     `json:"truncated_before"`
	Gap             bool       `json:"gap"`
	Error           string     `json:"error,omitempty"`
}
type JobChunk struct {
	Cursor     uint64 `json:"cursor"`
	NextCursor uint64 `json:"next_cursor"`
	Stream     string `json:"stream"`
	Data       string `json:"data"`
}
