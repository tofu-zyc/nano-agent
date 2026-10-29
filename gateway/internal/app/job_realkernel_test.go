package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/proto"
)

// Enable only this contract fixture until the production durable inbox is
// wired. A production App must not acknowledge Job ownership without it.
type jobContractHandler struct {
	*App
	observed chan proto.Msg
}

func (*jobContractHandler) JobProtocol() int { return 1 }
func (h *jobContractHandler) KernelMsg(node string, m *proto.Msg) {
	if m.T == proto.TJobState || m.T == proto.TJobsSync {
		h.observed <- *m
	}
	h.App.KernelMsg(node, m)
}
func TestRealCJobControlFramesThroughGoHub(t *testing.T) {
	var handler *jobContractHandler
	e, _ := setupRealKernel(t, func(a *App) kernel.Handler {
		handler = &jobContractHandler{App: a, observed: make(chan proto.Msg, 128)}
		return handler
	})
	e.a.mu.Lock()
	nodeName := ""
	for name := range e.a.nodes {
		nodeName = name
		break
	}
	e.a.mu.Unlock()
	work := filepath.Join(e.dir, "job-work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(work, "release")
	req, _ := json.Marshal(map[string]any{
		"tool": "Shell", "args": map[string]any{"command": "printf real-job; while [ ! -f '" + gate + "' ]; do sleep 0.02; done; printf done", "execution": "background"},
		"origin": map[string]any{"agent": "default", "sid": "job-contract-source", "run_id": "job-contract-run", "turn_id": 1, "call_id": "shell", "created_by": "fake:u1"},
		"dirs":   []string{work}, "write": true, "sandbox": "off",
	})
	ctx, cancel := context.WithTimeout(e.a.ctx, 10*time.Second)
	defer cancel()
	result := (kernelHost{a: e.a, node: nodeName}).Exec(ctx, req)
	if !result.OK {
		t.Fatal(result)
	}
	var receipt struct{ Status, JobID string }
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result.Content), &raw); err != nil {
		t.Fatal(err, result)
	}
	json.Unmarshal(raw["status"], &receipt.Status)
	json.Unmarshal(raw["job_id"], &receipt.JobID)
	if receipt.Status != "backgrounded" || receipt.JobID == "" {
		t.Fatal(result)
	}
	// Returning from the original host request detaches its cancellation context.
	cancel()
	await := func(predicate func(proto.Msg) bool) proto.Msg {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case message := <-handler.observed:
				if predicate(message) {
					return message
				}
			case <-timer.C:
				t.Fatal("Job frame missing through real Go decoder")
				return proto.Msg{}
			}
		}
	}
	control := func(action, request string) proto.Msg {
		t.Helper()
		if !e.a.hub.Send(nodeName, proto.Msg{T: proto.TJobControl, RequestID: request, Action: action, JobID: receipt.JobID, MaxBytes: 65536}) {
			t.Fatal("node lost")
		}
		return await(func(m proto.Msg) bool { return m.T == proto.TJobState && m.RequestID == request })
	}
	var running proto.Msg
	deadline := time.Now().Add(5 * time.Second)
	for {
		running = control("status", "status")
		if running.OK == nil || !*running.OK || running.Job == nil {
			t.Fatal(running)
		}
		if running.Job.PID > 0 && running.Job.OutputCursor >= 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real command did not start", running)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if running.Job.Status != "running" || !running.Job.Background || running.Job.CreatedBy != "fake:u1" ||
		running.Job.OwnerAgent != "default" || running.Job.Origin.SID != "job-contract-source" ||
		running.Job.Lease == nil || running.Job.Lease.JobID != receipt.JobID {
		t.Fatal(running.Job)
	}
	output := control("output", "output")
	var log proto.JobOutput
	if err := json.Unmarshal(output.JobData, &log); err != nil {
		t.Fatal(err, output)
	}
	var bytes []byte
	for _, chunk := range log.Chunks {
		data, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			t.Fatal(err)
		}
		bytes = append(bytes, data...)
	}
	if string(bytes) != "real-job" || log.NextCursor != 8 {
		t.Fatal(string(bytes), log)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	terminal := await(func(m proto.Msg) bool { return m.Kind == "terminal" && m.JobID == receipt.JobID })
	if terminal.Job == nil || terminal.Job.Status != "succeeded" || terminal.EventSeq != 1 ||
		terminal.Job.Result == nil || !terminal.Job.Result.OK || terminal.Job.OutputCursor != 12 {
		t.Fatal(terminal)
	}
	e.a.hub.Send(nodeName, proto.Msg{T: proto.TEventsAck, JobID: receipt.JobID, EventSeq: 1})
	ack := await(func(m proto.Msg) bool { return m.Kind == "ack" && m.JobID == receipt.JobID })
	if ack.Job == nil || ack.Job.NotificationState != "delivered" {
		t.Fatal(ack)
	}
	// A typed array projection is distinct from the string data used by pipes.
	listing := control("list", "list")
	var rows []proto.JobSnapshot
	if err := json.Unmarshal(listing.JobData, &rows); err != nil || len(rows) != 1 || rows[0].JobID != receipt.JobID {
		t.Fatal(rows, err)
	}
}
