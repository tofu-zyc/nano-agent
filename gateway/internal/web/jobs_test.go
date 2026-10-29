package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/eventlog"
	"nano-agent/gateway/internal/kernel"
	"nano-agent/gateway/internal/proto"
)

const webJobEpoch = "00112233445566778899aabbccddeeff"

func webJob(id string) eventlog.Job {
	lease := proto.LeaseSpec{LeaseID: "lease-" + id, JobID: "j_" + webJobEpoch + "-" + id, Agent: "default", CreatedBy: "cli:local",
		SID: "branch:quiet", RunID: "run", TurnID: 3, CallID: id, Name: "Shell", Silent: true, Exclusive: "files:/secret"}
	return eventlog.Job{SourceID: "webnode", Creator: lease.CreatedBy, Silent: true, Receipt: proto.JobSnapshot{
		Version: 1, JobID: lease.JobID, NodeEpoch: webJobEpoch, OwnerAgent: "default", CreatedBy: lease.CreatedBy,
		OriginSID: lease.SID, OriginRunID: lease.RunID, OriginCallID: id, Origin: lease, Lease: &lease,
		Status: "running", Execution: "auto", Revision: 1, CreatedAt: 100, StartedAt: 101, TimeoutSeconds: 1800,
		OutputTail: "private-output-preview"}}
}

func TestJobsHTTPAuthenticationPaginationAndOfflineState(t *testing.T) {
	srv, _, _ := setupSeededJobs(t, []eventlog.Job{webJob("1"), webJob("2")})
	c := newClient(t, srv)
	if code, _ := c.do("GET", "/api/jobs", nil, false); code != 401 {
		t.Fatal(code)
	}
	if code, _ := c.do("POST", "/api/jobs/j_00112233445566778899aabbccddeeff-1/control", map[string]any{"action": "status"}, true); code != 401 {
		t.Fatal(code)
	}
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	code, out := c.do("GET", "/api/jobs?limit=1", nil, false)
	if code != 200 {
		t.Fatal(code, out)
	}
	rows, ok := out["jobs"].([]any)
	if !ok || len(rows) != 1 || out["next"] == nil {
		t.Fatal(out)
	}
	pageCode, page := c.do("GET", "/api/jobs?limit=1&after="+url.QueryEscape(out["next"].(string)), nil, false)
	if pageCode != 200 || page["next"] != nil || page["jobs"].([]any)[0].(map[string]any)["origin_call_id"] != "2" {
		t.Fatal("pagination skipped or repeated a task", pageCode, page)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "private-output") || strings.Contains(string(raw), "/secret") || strings.Contains(string(raw), "lease-") {
		t.Fatal("list leaked executor body", string(raw))
	}
	if row := rows[0].(map[string]any); row["online"] != false || row["silent"] != true || row["origin_call_id"] != "1" {
		t.Fatal(row)
	}
	if code, out = c.do("POST", "/api/jobs/j_00112233445566778899aabbccddeeff-1/control", map[string]any{"action": "status", "node": "webnode"}, true); code != 200 || out["job"] == nil {
		t.Fatal(code, out)
	}
	if code, _ = c.do("POST", "/api/jobs/j_00112233445566778899aabbccddeeff-1/control", map[string]any{"action": "cancel", "node": "webnode"}, true); code != 400 {
		t.Fatal("offline cancel falsely accepted", code)
	}
	if code, _ = c.do("POST", "/api/jobs/j_00112233445566778899aabbccddeeff-1/control", map[string]any{"action": "status"}, false); code != 403 {
		t.Fatal("unguarded control", code)
	}
	for _, query := range []string{"limit=wrong", "limit=101"} {
		if code, _ = c.do("GET", "/api/jobs?"+query, nil, false); code != 400 {
			t.Fatal(code, query)
		}
	}
	for _, body := range []any{
		map[string]any{"action": "list"},
		map[string]any{"action": "output", "cursor": 9007199254740992},
		map[string]any{"action": "status", "unexpected": true},
		map[string]any{"action": "status", "node": strings.Repeat("x", 4200)},
	} {
		if code, _ = c.do("POST", "/api/jobs/j_00112233445566778899aabbccddeeff-1/control", body, true); code != 400 {
			t.Fatal(code, body)
		}
	}
}

func TestJobsHTTPControlsUseNodeProtocolAndCompletionSSE(t *testing.T) {
	j := webJob("3")
	srv, s, _ := setupSeededJobs(t, []eventlog.Job{j})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := kernel.NewHub("test", s.app, log)
	s.app.SetHub(hub)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go hub.Serve(ctx, ln)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	encoder := json.NewEncoder(conn)
	if err = encoder.Encode(proto.Msg{T: proto.THello, V: proto.Version, Node: "webnode", Token: "test", NodeEpoch: webJobEpoch,
		Features: map[string]json.RawMessage{"job_manager": json.RawMessage("1"), "job_stream": json.RawMessage("1")}}); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(conn)
	read := func(want string) proto.Msg {
		t.Helper()
		conn.SetReadDeadline(time.Now().Add(4 * time.Second))
		for scanner.Scan() {
			var msg proto.Msg
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				t.Fatal(err)
			}
			if msg.T == want {
				return msg
			}
		}
		t.Fatal("missing node frame", want, scanner.Err())
		return proto.Msg{}
	}
	read(proto.TWelcome)
	c := newClient(t, srv)
	c.do("POST", "/api/login", map[string]string{"password": "pw"}, true)
	requestCtx, requestCancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer requestCancel()
	req, _ := http.NewRequestWithContext(requestCtx, "GET", srv.URL+"/api/events", nil)
	stream, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	events := bufio.NewScanner(stream.Body)
	if !events.Scan() {
		t.Fatal("no SSE greeting")
	}
	type response struct {
		code int
		out  map[string]any
	}
	control := func(action string) response {
		t.Helper()
		done := make(chan response, 1)
		go func() {
			code, out := c.do("POST", "/api/jobs/"+j.Receipt.JobID+"/control", map[string]any{"node": "webnode", "action": action, "cursor": 4}, true)
			done <- response{code, out}
		}()
		msg := read(proto.TJobControl)
		if msg.Action != action || msg.JobID != j.Receipt.JobID || msg.RequestID == "" {
			t.Fatal(msg)
		}
		var raw json.RawMessage
		if action == "output" {
			if msg.Cursor != 4 {
				t.Fatal(msg)
			}
			raw = json.RawMessage(`{"chunks":[{"cursor":4,"next_cursor":9,"stream":"stdout","data":"aGVsbG8="}],"next_cursor":9,"output_cursor":9,"truncated_before":0,"gap":false}`)
		}
		if err := encoder.Encode(proto.Msg{T: proto.TJobState, NodeEpoch: webJobEpoch, RequestID: msg.RequestID, JobID: j.Receipt.JobID, OK: proto.Bool(true), JobData: raw}); err != nil {
			t.Fatal(err)
		}
		select {
		case result := <-done:
			if result.code != 200 || result.out["accepted"] != true {
				t.Fatal(result)
			}
			return result
		case <-time.After(4 * time.Second):
			t.Fatal("control HTTP did not finish")
		}
		return response{}
	}
	result := control("output")
	if result.out["output"].(map[string]any)["next_cursor"] != float64(9) {
		t.Fatal(result)
	}
	control("detach")
	control("cancel")
	j.Receipt.Status = "cancelled"
	j.Receipt.Revision++
	j.Receipt.EndedAt = 200
	j.Receipt.LastEventSeq = 1
	j.Receipt.Result = &proto.JobResult{Cancelled: true, Content: "private-result-body"}
	if err := encoder.Encode(proto.Msg{T: proto.TJobState, NodeEpoch: webJobEpoch, JobID: j.Receipt.JobID, Job: &j.Receipt, EventSeq: 1}); err != nil {
		t.Fatal(err)
	}
	ack := read(proto.TEventsAck)
	if ack.JobID != j.Receipt.JobID || ack.EventSeq != 1 {
		t.Fatal(ack)
	}
	found := false
	for events.Scan() {
		line := events.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			T    string         `json:"t"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.T != "job" {
			continue
		}
		if event.Data["status"] != "cancelled" || event.Data["silent"] != true || event.Data["origin_call_id"] != "3" {
			t.Fatal(event)
		}
		if strings.Contains(line, "private-result") || strings.Contains(line, "private-output") || strings.Contains(line, "/secret") {
			t.Fatal("SSE exposed execution payload", line)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("no durable completion SSE", events.Err())
	}
	code, out := c.do("POST", "/api/jobs/"+j.Receipt.JobID+"/control", map[string]any{"action": "wait", "wait_ms": 100}, true)
	if code != 200 || out["job"].(map[string]any)["status"] != "cancelled" || out["wait_expired"] == true {
		t.Fatal(code, out)
	}
}
