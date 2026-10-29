package app

// 有代表性的资源实测（设计 §10 / §12 的收口项）：并行输出、Job 日志、Hook 队列和
// 100 个闲置分支，分别看累计分配（TotalAlloc）、活跃堆（HeapAlloc）和进程内存（RSS）。

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/hooks"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/proto"
	"nano-agent/gateway/internal/session"
)

type memSnap struct {
	HeapAlloc  uint64
	TotalAlloc uint64
	RSS        uint64
}

func memSnapshot() memSnap {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	snap := memSnap{HeapAlloc: m.HeapAlloc, TotalAlloc: m.TotalAlloc}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "VmRSS:") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					snap.RSS = kb << 10
				}
			}
		}
	}
	return snap
}

func memDelta(before, after memSnap) (heap, total, rss uint64) {
	sub := func(a, b uint64) uint64 {
		if a < b {
			return 0
		}
		return a - b
	}
	return sub(after.HeapAlloc, before.HeapAlloc), sub(after.TotalAlloc, before.TotalAlloc), sub(after.RSS, before.RSS)
}

// 100 个闲置分支：建好、刷新列表之后，活跃堆和进程内存都要有界，不能把分支历史常驻。
func TestPerfHundredIdleBranchesMemory(t *testing.T) {
	e := setup(t)
	if err := e.a.store.Append(mainSID, session.Msg{Role: "user", Content: "种子"}); err != nil {
		t.Fatal(err)
	}
	first, err := e.a.SessionFork(SessionForkRequest{SourceSID: mainSID, Title: "0", RequestID: "perf-0", By: "fake:u1"})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := e.a.store.Snapshot(first.SnapshotID)
	e.a.store.UnloadHistory(first.BranchSID)
	before := memSnapshot()
	for i := 1; i < 100; i++ {
		sid := "br-perf-" + strconv.Itoa(i)
		meta := session.BranchMeta{SID: sid, ParentSID: mainSID, Title: "闲置", Status: session.BranchPreparing, Delivery: session.BranchDeliverySilent}
		if err := e.a.store.BeginBranch(meta); err != nil {
			t.Fatal(err)
		}
		if err := e.a.store.ReadyBranch(sid, snap, session.BranchReady); err != nil {
			t.Fatal(err)
		}
	}
	list := e.a.Branches()
	if len(list) < 100 {
		t.Fatalf("分支数：%d", len(list))
	}
	after := memSnapshot()
	heap, total, rss := memDelta(before, after)
	t.Logf("100 个闲置分支：heap=%d MiB total=%d MiB rss=%d MiB", heap>>20, total>>20, rss>>20)
	if heap > 32<<20 {
		t.Fatalf("闲置分支常驻内存过多：%d MiB", heap>>20)
	}
	if rss > 128<<20 {
		t.Fatalf("进程内存增长过多：%d MiB", rss>>20)
	}
}

// 并行工具输出乱序、大结果：都进历史后内存仍然有界，且每条结果都配对正确。
type parallelPerfModel struct{ asked int }

func (m *parallelPerfModel) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	m.asked++
	if m.asked > 1 {
		return &llm.Result{Reason: llm.ReasonFinal, Content: "三个都读完了"}, nil
	}
	var calls []llm.ToolCall
	for _, id := range []string{"p1", "p2", "p3"} {
		calls = append(calls, llm.ToolCall{ID: id, Type: "function",
			Function: llm.FunctionCall{Name: "Read", Arguments: `{"path":"` + id + `.txt"}`}})
	}
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: calls}, nil
}

func TestPerfParallelOutputStaysBounded(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.llm = &parallelPerfModel{}
	e.a.mu.Unlock()
	before := memSnapshot()
	e.say("并行读三个大文件")
	e.k.expect(proto.TInput)
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	turn := e.k.expect(proto.TTurn)
	if len(turn.Calls) != 3 {
		t.Fatalf("测试模型没有发出三个调用：%+v", turn.Calls)
	}
	big := strings.Repeat("x", 1<<20)
	for i := len(turn.Calls) - 1; i >= 0; i-- { // 结果倒序回来
		e.k.send(proto.Msg{T: proto.TResult, SID: mainSID, ID: turn.Calls[i].ID, OK: proto.Bool(true), Content: big})
	}
	e.k.send(proto.Msg{T: proto.TModel, SID: mainSID})
	e.k.expect(proto.TTurn)
	e.k.send(proto.Msg{T: proto.TDone, SID: mainSID, Status: proto.StatusOK})
	waitIdle(t, e, mainSID)
	tools := 0
	for _, m := range e.a.store.History(mainSID) {
		if m.Role != "tool" {
			continue
		}
		tools++
		if len(m.Content) < 1<<20 {
			t.Fatalf("大结果被截断了：%d", len(m.Content))
		}
	}
	if tools != 3 {
		t.Fatalf("工具结果条数：%d", tools)
	}
	after := memSnapshot()
	heap, total, rss := memDelta(before, after)
	t.Logf("3 路并行 1 MiB 乱序输出：heap=%d MiB total=%d MiB rss=%d MiB", heap>>20, total>>20, rss>>20)
	if heap > 64<<20 {
		t.Fatalf("并行输出常驻内存过多：%d MiB", heap>>20)
	}
}

// Hook：一批事件经可靠日志匹配并投递，记录吞吐和内存；队列和账本都要有界。
func TestPerfHookEventBurstMemory(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	e.a.eps["fake:c1"] = &epRec{Session: mainSID, Who: "fake:u1"}
	e.a.mu.Unlock()
	spec := hooks.Spec{Name: "批量", On: []string{"device.online"}, Handler: hooks.Handler{Type: "notify", Text: "事件 {{event_name}}"}, Deliver: "none"}
	if _, err := e.a.HookAction(context.Background(), mainSID, "fake:u1", HookRequest{Action: "create", OperationID: "burst", Spec: &spec}); err != nil {
		t.Fatal(err)
	}
	// 每分钟每个 Hook 最多 16 次触发是既有设计，这里验证的是队列和日志的内存边界，
	// 不是绕过限额：事件都进可靠日志，投递按限额放行。
	const n = 200
	before := memSnapshot()
	start := time.Now()
	for i := 0; i < n; i++ {
		data, _ := json.Marshal(map[string]string{"path": "burst-" + strconv.Itoa(i) + ".log"})
		ev := eventlog.Event{Name: "device.online", AgentID: "default", OriginSID: mainSID, SourceID: "perf",
			SourceEpoch: "boot", Seq: uint64(i + 1), OccurredAt: 100, ObservedAt: 100, Data: data}
		ev.ID = eventlog.EventID(ev.SourceID, ev.SourceEpoch, ev.Seq)
		if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Event: &ev}); err != nil {
			t.Fatal(err)
		}
	}
	e.a.hookEngine.Wake()
	deadline := time.Now().Add(30 * time.Second)
	delivered := 0
	for {
		rows, _ := e.a.jobEvents.Deliveries("", 256)
		delivered = 0
		for _, d := range rows {
			if d.Status == "succeeded" {
				delivered++
			}
		}
		// 限额是每分钟 16 次：等到至少 12 条确认处理完，剩下的按限额排队
		if delivered >= 12 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if delivered < 12 {
		t.Fatalf("Hook 只处理完 %d 条", delivered)
	}
	elapsed := time.Since(start)
	after := memSnapshot()
	heap, total, rss := memDelta(before, after)
	t.Logf("%d 条事件入可靠日志、%d 条按每分钟 16 次限额投递：%s，heap=%d MiB total=%d MiB rss=%d MiB",
		n, delivered, elapsed.Round(time.Millisecond), heap>>20, total>>20, rss>>20)
	if heap > 64<<20 {
		t.Fatalf("Hook 处理常驻内存过多：%d MiB", heap>>20)
	}
}

const perfJobEpoch = "ffeeddccbbaa99887766554433221100"

// Job 索引：一批终态任务落进可靠日志后，按游标翻页的内存有界，不会把每条输出常驻。
func TestPerfJobIndexStaysBounded(t *testing.T) {
	e := setup(t)
	const n = 200
	before := memSnapshot()
	for i := 0; i < n; i++ {
		ev := eventlog.Event{Name: "device.online", AgentID: "default", SourceID: "perf",
			SourceEpoch: "boot", Seq: uint64(i + 1), OccurredAt: 100, ObservedAt: 100}
		ev.ID = eventlog.EventID(ev.SourceID, ev.SourceEpoch, ev.Seq)
		id := strconv.Itoa(i)
		lease := proto.LeaseSpec{LeaseID: "lease-" + id, JobID: "j_" + perfJobEpoch + "-" + id, Agent: "default",
			CreatedBy: "fake:u1", SID: mainSID, RunID: "run", TurnID: 1, CallID: "c" + id, Name: "Shell", Exclusive: "files:/x"}
		job := eventlog.Job{SourceID: "perf", Creator: lease.CreatedBy, Receipt: proto.JobSnapshot{
			Version: 1, JobID: lease.JobID, NodeEpoch: perfJobEpoch, OwnerAgent: "default", CreatedBy: lease.CreatedBy,
			OriginSID: lease.SID, OriginRunID: lease.RunID, OriginCallID: lease.CallID, Origin: lease, Lease: &lease,
			Status: "running", Execution: "auto", Revision: 1, CreatedAt: 100, StartedAt: 101, TimeoutSeconds: 1800}}
		if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Admit: true, Job: &job}); err != nil {
			t.Fatal(err)
		}
		if err := e.a.jobEvents.Commit(context.Background(), eventlog.Transaction{Event: &ev}); err != nil {
			t.Fatal(err)
		}
	}
	cursor, total := "", 0
	for {
		rows, next := e.a.jobEvents.Jobs(cursor, 50)
		total += len(rows)
		if next == "" {
			break
		}
		cursor = next
	}
	if total < n {
		t.Fatalf("Job 索引翻页只有 %d/%d 条", total, n)
	}
	after := memSnapshot()
	heap, alloc, rss := memDelta(before, after)
	t.Logf("%d 个终态 Job 的索引与翻页：heap=%d MiB total=%d MiB rss=%d MiB", n, heap>>20, alloc>>20, rss>>20)
	if heap > 32<<20 {
		t.Fatalf("Job 索引常驻内存过多：%d MiB", heap>>20)
	}
}
