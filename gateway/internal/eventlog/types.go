// Package eventlog owns the reliable runtime inbox. It is deliberately separate
// from events.Bus: accepted changes are published only after a single journal
// transaction has been synced. Callers authorize sources before submission.
package eventlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nano-agent/gateway/internal/proto"
)

var (
	ErrBusy     = errors.New("可靠事件队列已满，来源必须保留未确认事件")
	ErrClosed   = errors.New("可靠事件日志已关闭")
	ErrCapacity = errors.New("可靠事件存储容量不足，暂停接收新任务")
)

type Limits struct {
	Jobs, Events, Deliveries, Sources, Queue int
	LiveBytes, JournalBytes, CompactBytes    int64
}

func (l Limits) defaults() Limits {
	if l.Jobs <= 0 {
		l.Jobs = 1024
	}
	if l.Events <= 0 {
		l.Events = 2048
	}
	if l.Deliveries <= 0 {
		l.Deliveries = 4096
	}
	if l.Sources <= 0 {
		l.Sources = 4096
	}
	if l.Queue <= 0 {
		l.Queue = 128
	}
	if l.LiveBytes <= 0 {
		l.LiveBytes = 16 << 20
	}
	if l.JournalBytes <= 0 {
		l.JournalBytes = 64 << 20
	}
	if l.CompactBytes <= 0 {
		l.CompactBytes = 1 << 20
	}
	return l
}

type Event struct {
	OriginSID   string          `json:"origin_sid,omitempty"`
	ID          string          `json:"event_id"`
	Name        string          `json:"name"`
	SourceID    string          `json:"source_id"`
	SourceEpoch string          `json:"source_epoch"`
	Seq         uint64          `json:"seq"`
	OccurredAt  int64           `json:"occurred_at"`
	ObservedAt  int64           `json:"observed_at"`
	AgentID     string          `json:"agent_id"`
	RunID       string          `json:"run_id,omitempty"`
	JobID       string          `json:"job_id,omitempty"`
	CausationID string          `json:"causation_id,omitempty"`
	OriginHook  string          `json:"origin_hook,omitempty"`
	Depth       int             `json:"depth"`
	Silent      bool            `json:"silent,omitempty"`
	DataRef     string          `json:"data_ref,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

func EventID(source, epoch string, seq uint64) string {
	raw, _ := json.Marshal([]any{source, epoch, seq})
	hash := sha256.Sum256(raw)
	return "ev_" + hex.EncodeToString(hash[:])
}
func (e Event) valid() error {
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 2048 || e.ID != EventID(e.SourceID, e.SourceEpoch, e.Seq) ||
		e.Name == "" || len(e.Name) > 96 || e.SourceID == "" || len(e.SourceID) > 256 ||
		e.SourceEpoch == "" || len(e.SourceEpoch) > 128 || e.Seq == 0 ||
		e.OccurredAt <= 0 || e.ObservedAt <= 0 || e.AgentID == "" ||
		e.Depth < 0 || e.Depth > 4 {
		return errors.New("可靠事件的身份、正文或因果深度无效")
	}
	return nil
}

// Job freezes the Gateway-authorized source identity. The node's public
// projection contains no executable command; it is never replayed by this store.
type Job struct {
	SourceID string            `json:"source_id"`
	Creator  string            `json:"creator"`
	Silent   bool              `json:"silent"`
	Receipt  proto.JobSnapshot `json:"receipt"`
}
type JobRecord struct {
	Job
	Reserved        bool   `json:"terminal_reserved"`
	TerminalEventID string `json:"terminal_event_id,omitempty"`
}
type EventRecord struct {
	Event
	Matched     bool     `json:"matched"`
	DeliveryIDs []string `json:"deliveries,omitempty"`
}
type Delivery struct {
	RunSID     string          `json:"run_sid,omitempty"`
	ObjectKey  string          `json:"object_key,omitempty"`
	UpdatedMS  int64           `json:"updated_ms,omitempty"`
	Registered bool            `json:"registered_rule,omitempty"`
	ID         string          `json:"delivery_id"`
	EventID    string          `json:"event_id"`
	HookID     string          `json:"hook_id"`
	Revision   uint64          `json:"hook_revision"`
	AgentID    string          `json:"agent_id"`
	Creator    string          `json:"creator"`
	Silent     bool            `json:"silent"`
	TargetSID  string          `json:"target_sid,omitempty"`
	Rule       json.RawMessage `json:"rule"`
	Status     string          `json:"status"`
	Attempt    int             `json:"attempt"`
	NextAt     int64           `json:"next_at,omitempty"`
	Error      string          `json:"error,omitempty"`
	Result     string          `json:"result,omitempty"`
}

func DeliveryID(event, hook string, revision uint64) string {
	raw, _ := json.Marshal([]any{event, hook, revision})
	hash := sha256.Sum256(raw)
	return "hd_" + hex.EncodeToString(hash[:])
}
func (d Delivery) valid() error {
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 8192 || d.ID != DeliveryID(d.EventID, d.HookID, d.Revision) ||
		d.EventID == "" || d.HookID == "" || len(d.HookID) > 128 || d.Revision == 0 ||
		d.AgentID == "" || len(d.RunSID) > 256 || d.Attempt < 0 || d.Attempt > 8 || !json.Valid(d.Rule) {
		return errors.New("Hook 投递身份、规则快照或重试次数无效")
	}
	switch d.Status {
	case "pending", "running", "retry", "succeeded", "failed", "uncertain", "cancelled":
		return nil
	}
	return errors.New("Hook 投递状态无效")
}
func deliveryDone(status string) bool {
	return status == "succeeded" || status == "failed" || status == "uncertain" || status == "cancelled"
}

type Match struct {
	Coalesce   bool       `json:"coalesce,omitempty"`
	Checked    bool       `json:"check_registered_rules,omitempty"`
	EventID    string     `json:"event_id"`
	Deliveries []Delivery `json:"deliveries"`
}

// Transaction updates source state and its event atomically. Admit may only be
// set after the caller has verified a live invocation or a durable recovery
// proof. An observation alone cannot activate an unknown Job.
type Transaction struct {
	Rule     *RuleChange `json:"rule,omitempty"`
	Admit    bool        `json:"admit,omitempty"`
	Job      *Job        `json:"job,omitempty"`
	Event    *Event      `json:"event,omitempty"`
	Match    *Match      `json:"match,omitempty"`
	Delivery *Delivery   `json:"delivery,omitempty"`
	// Forget removes already matched events and completed delivery bodies.
	// The Job's terminal identity stays as the deduplication receipt.
	Forget []string `json:"forget,omitempty"`
}
type Stats struct {
	Jobs, Events, Deliveries, Sources, Rules, Reserved, Queued int
	LiveBytes, JournalBytes, ReservedBytes                     int64
	Err                                                        error
}

func terminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "timed_out", "interrupted", "unknown":
		return true
	}
	return false
}
func jobKey(j Job) string { return cursorKey(j.SourceID, j.Receipt.JobID) }
func jobValid(j Job) error {
	q := j.Receipt
	raw, err := json.Marshal(j)
	if err != nil || len(raw) > 32<<10 || j.SourceID == "" || len(j.SourceID) > 128 ||
		!strings.HasPrefix(q.JobID, "j_"+q.NodeEpoch+"-") || len(q.JobID) > 96 ||
		q.NodeEpoch == "" || q.OwnerAgent == "" || q.OriginSID == "" || q.CreatedAt <= 0 ||
		q.Origin.Agent != q.OwnerAgent || q.Origin.SID != q.OriginSID ||
		q.Origin.RunID != q.OriginRunID || q.Origin.CallID != q.OriginCallID ||
		q.CreatedBy != j.Creator || q.Origin.CreatedBy != j.Creator || q.Origin.Silent != j.Silent {
		return errors.New("任务固定身份或状态正文无效")
	}
	switch q.Status {
	case "preparing", "queued", "starting", "running", "cancelling":
	default:
		if !terminal(q.Status) {
			return errors.New("任务状态无效")
		}
	}
	return nil
}
func identity(j Job) string {
	q := j.Receipt
	lease := q.Lease
	// Mutable lease fields do not change the immutable invocation identity.
	if lease != nil {
		c := *lease
		c.Background = false
		c.State = ""
		c.DetachRequest = ""
		c.TerminalReceipt = false
		lease = &c
	}
	raw, _ := json.Marshal([]any{j.SourceID, j.Creator, j.Silent, q.JobID, q.NodeEpoch,
		q.OwnerAgent, q.OriginSID, q.OriginRunID, q.OriginCallID, q.CreatedAt, q.Origin, lease})
	return string(raw)
}
func sameEvent(a, b Event) bool {
	a.ObservedAt = 0
	b.ObservedAt = 0
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func sameDelivery(a, b Delivery) bool {
	if a.RunSID != "" && a.RunSID != b.RunSID {
		return false
	}
	a.RunSID = ""
	b.RunSID = ""
	a.Status = ""
	b.Status = ""
	a.Attempt = 0
	b.Attempt = 0
	a.NextAt = 0
	b.NextAt = 0
	a.UpdatedMS = 0
	b.UpdatedMS = 0
	a.Error = ""
	b.Error = ""
	a.Result = ""
	b.Result = ""
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func jobEventName(status string) string {
	switch status {
	case "succeeded":
		return "job.completed"
	case "cancelled":
		return "job.cancelled"
	case "timed_out":
		return "job.timed_out"
	default:
		return "job.failed"
	}
}
func eventMatchesJob(e Event, j Job) error {
	q := j.Receipt
	if e.JobID != q.JobID || e.SourceID != j.SourceID+"/"+q.JobID ||
		e.SourceEpoch != q.NodeEpoch || e.Seq != q.LastEventSeq || e.AgentID != q.OwnerAgent ||
		e.RunID != q.OriginRunID || e.Silent != j.Silent || e.Name != jobEventName(q.Status) || e.OccurredAt != q.EndedAt {
		return fmt.Errorf("任务终态与同一事务的事件不一致")
	}
	return nil
}

// A terminal retry may advance its durable ACK/revision and log observation,
// but cannot replace the actual outcome, end time or frozen execution policy.
func sameTerminal(a, b proto.JobSnapshot) bool {
	a.Revision = 0
	b.Revision = 0
	a.NotificationState = ""
	b.NotificationState = ""
	a.OutputCursor = 0
	b.OutputCursor = 0
	a.OutputTail = ""
	b.OutputTail = ""
	a.OutputGap = false
	b.OutputGap = false
	a.LogError = ""
	b.LogError = ""
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func reservedBytes(j JobRecord) int64 {
	if !j.Reserved {
		return 0
	}
	// The record wrapper and final event have their own allowance. Other
	// events/deliveries cannot consume a live Job's guaranteed terminal bytes.
	return (34 << 10) + 2048 - size(j)
}

// Cursor keeps bounded out-of-order receipts for a sequential source stream.
// Through survives event-body retention, so reconnects cannot re-run an old
// notification. A source starts at seq=1 within its fixed epoch.
type Cursor struct {
	SourceID string   `json:"source_id"`
	Epoch    string   `json:"source_epoch"`
	Through  uint64   `json:"through"`
	Seen     []uint64 `json:"seen,omitempty"`
}

func cursorKey(source, epoch string) string {
	raw, _ := json.Marshal([]string{source, epoch})
	return string(raw)
}
func (c Cursor) contains(seq uint64) bool {
	if seq <= c.Through {
		return true
	}
	for _, seen := range c.Seen {
		if seen == seq {
			return true
		}
	}
	return false
}
func (c Cursor) valid() bool {
	if c.SourceID == "" || len(c.SourceID) > 256 || c.Epoch == "" || len(c.Epoch) > 128 || len(c.Seen) > 128 {
		return false
	}
	previous := c.Through
	for _, seq := range c.Seen {
		if seq <= previous || seq-c.Through > 4096 {
			return false
		}
		previous = seq
	}
	return true
}

func phase(status string) int {
	switch status {
	case "preparing":
		return 0
	case "queued":
		return 1
	case "starting":
		return 2
	case "running":
		return 3
	case "cancelling":
		return 4
	}
	return 5
}
