package dispatch

import (
	"fmt"
	"testing"
	"time"
)

func newTest(t *testing.T, slots int) *Scheduler {
	t.Helper()
	cfg := Defaults()
	cfg.Slots, cfg.HookReserve = slots, 0
	cfg.Shells = slots
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func request(id, sid string) Request {
	return Request{ID: id, Owner: "node/boot", Machine: "machine", Session: sid, RunID: "run-" + sid, TurnID: 1, CallID: id, Agent: "agent"}
}
func admit(t *testing.T, s *Scheduler, r Request, now time.Time) Lease {
	t.Helper()
	l, _, err := s.Admit(r, now)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestFairRotationAgingAndBoundedAdmission(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newTest(t, 1)
	admit(t, s, request("a1", "a"), now)
	for i := 2; i < 5; i++ {
		admit(t, s, request(fmt.Sprint("a", i), "a"), now)
	}
	admit(t, s, request("b1", "b"), now)
	low := request("low", "c")
	low.Priority = 10
	admit(t, s, low, now)
	next, err := s.Release("a1", "node/boot", now)
	if err != nil || len(next) != 1 {
		t.Fatal(next, err)
	}
	next, err = s.Release(next[0].ID, "node/boot", now)
	if err != nil || len(next) != 1 || next[0].Session != "b" {
		t.Fatal("one session monopolized slots", next, err)
	}
	next, err = s.Release("b1", "node/boot", now.Add(11*time.Second))
	if err != nil || len(next) != 1 {
		t.Fatal(next, err)
	}
	// The aged autonomous session joins the same fair round and cannot remain
	// permanently behind a stream of new foreground calls.
	for i := 0; i < 4 && next[0].ID != "low"; i++ {
		admit(t, s, request(fmt.Sprint("new", i), fmt.Sprint("new-session", i)), now.Add(11*time.Second))
		next, err = s.Release(next[0].ID, "node/boot", now.Add(11*time.Second))
		if err != nil || len(next) != 1 {
			t.Fatal(next, err)
		}
	}
	if next[0].ID != "low" {
		t.Fatal("autonomous request starved", next)
	}
	for i := 0; i < 30; i++ {
		_, _, _ = s.Admit(request(fmt.Sprint("bounded", i), "queued"), now)
	}
	if len(s.Snapshot()) > s.cfg.Live {
		t.Fatal("machine live count grew past bound")
	}
}

func TestResourceHierarchyWindowsAliasesAndRealExitBarrier(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newTest(t, 8)
	first := request("shell", "one")
	first.Shell = true
	first.Windows = true
	first.Exclusive = []string{`dir:c:\工作\项目`}
	first.Shared = []string{"machine:files"}
	if admit(t, s, first, now).State != "granted" {
		t.Fatal("initial grant missing")
	}
	second := request("write", "two")
	second.Windows = true
	second.Shared = []string{`dir:C:/工作/项目/sub`, `machine:files`}
	second.Exclusive = []string{`file:C:/工作/项目/sub/a.txt`}
	l := admit(t, s, second, now)
	if l.State != "queued" || l.Holder != "shell" {
		t.Fatal("overlapping aliases did not conflict", l)
	}
	if _, err := s.Cancel("shell", "node/boot", now); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot()[1].State != "queued" {
		t.Fatal("cancel released a still-running process resource")
	}
	s.Disconnect("node/boot")
	snapshot := s.Snapshot()
	if len(snapshot) != 1 || snapshot[0].State != "unknown" {
		t.Fatal("disconnect forgot unknown process", snapshot)
	}
	third := second
	third.ID, third.Owner = "write-new", "other/boot"
	third.Session, third.RunID = "three", "run-three"
	if admit(t, s, third, now).State != "queued" {
		t.Fatal("disconnect released real resource")
	}
	grants, err := s.Release("shell", "node/boot", now)
	if err != nil || len(grants) != 1 || grants[0].ID != "write-new" {
		t.Fatal(grants, err)
	}
}

func TestDetachRetainsMachineLocksAndStopsCountingForegroundRunSlots(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := Defaults()
	cfg.PerRun = 1
	cfg.HookReserve = 0
	cfg.BackgroundPerAgent = 1
	s, _ := New(cfg)
	first := request("one", "sid")
	first.Exclusive = []string{"file:/a"}
	admit(t, s, first, now)
	detached, err := s.Detach("one", "node/boot")
	if err != nil || !detached.Background {
		t.Fatal(detached, err)
	}
	independent := request("two", "sid")
	independent.Exclusive = []string{"file:/b"}
	if admit(t, s, independent, now).State != "granted" {
		t.Fatal("detached job blocked independent foreground run")
	}
	blocked := request("three", "other")
	blocked.Shared = []string{"file:/a"}
	if admit(t, s, blocked, now).State != "queued" {
		t.Fatal("detach released a file lock")
	}
	if _, err := s.Detach("two", "node/boot"); err == nil {
		t.Fatal("background capacity bypassed")
	}
	for _, l := range s.Snapshot() {
		if l.ID == "two" && l.Background {
			t.Fatal("failed detach changed original foreground owner")
		}
	}
}

func TestHookPartitionAndPerRunLimitAcrossMachines(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := Defaults()
	cfg.Slots = 3
	cfg.PerRun = 2
	s, _ := New(cfg)
	admit(t, s, request("one", "a"), now)
	admit(t, s, request("two", "b"), now)
	if admit(t, s, request("three", "c"), now).State != "queued" {
		t.Fatal("normal work consumed reserved hook partition")
	}
	hook := request("hook", "hook")
	hook.Hook, hook.Shell = true, true
	if admit(t, s, hook, now).State != "granted" {
		t.Fatal("hook could not use reserved partition")
	}
	cross := request("cross1", "a")
	cross.Machine = "remote"
	admit(t, s, cross, now)
	cross.ID = "cross2"
	cross.CallID = "cross2"
	cross.Machine = "another"
	if admit(t, s, cross, now).State != "queued" {
		t.Fatal("per-run bound was applied separately on each machine")
	}
}

func TestLeaseIdentityAndDuplicateAdmission(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newTest(t, 2)
	req := request("same", "sid")
	admit(t, s, req, now)
	again, grants, err := s.Admit(req, now)
	if err != nil || again.State != "granted" || len(grants) != 0 || len(s.Snapshot()) != 1 {
		t.Fatal("duplicate acquired extra slot", again, grants, err)
	}
	changed := req
	changed.RunID = "other"
	if _, _, err := s.Admit(changed, now); err == nil {
		t.Fatal("another call claimed existing ID")
	}
	if _, err := s.Release("same", "foreign", now); err == nil {
		t.Fatal("foreign owner released lease")
	}
	if _, err := s.Release("same", "node/boot", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Admit(req, now); err == nil {
		t.Fatal("closed operation re-admitted")
	}
	if _, err := s.Release("same", "node/boot", now); err != nil {
		t.Fatal("duplicate release not idempotent", err)
	}
}

func TestDetachReservationDoesNotReleaseRunBudgetBeforeDurableHandoff(t *testing.T) {
	cfg := Defaults()
	cfg.HookReserve = 0
	cfg.PerRun = 1
	cfg.BackgroundPerAgent = 1
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	shell := request("shell", "source")
	admit(t, s, shell, now)
	sibling := shell
	sibling.ID = "sibling"
	sibling.CallID = "sibling"
	sibling.Machine = "other"
	if got := admit(t, s, sibling, now); got.State != "queued" {
		t.Fatal(got)
	}
	prepared, err := s.PrepareDetach(shell.ID, shell.Owner, "control-1")
	if err != nil || prepared.Background || prepared.DetachRequest != "control-1" {
		t.Fatal(prepared, err)
	}
	if grants := s.Pump(now); len(grants) != 0 {
		t.Fatal("prepare released foreground budget", grants)
	}
	bg := request("another-background", "other")
	bg.Background = true
	if _, _, err := s.Admit(bg, now); err == nil {
		t.Fatal("prepared capacity was offered to another background")
	}
	if _, _, err := s.CommitDetach(shell.ID, shell.Owner, "wrong-control", now); err == nil {
		t.Fatal("wrong handoff token committed")
	}
	if err := s.AbortDetach(shell.ID, shell.Owner, "control-1"); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Lookup(shell.ID)
	if current.Background || current.DetachRequest != "" {
		t.Fatal("failed handoff changed owner", current)
	}
	if grants := s.Pump(now); len(grants) != 0 {
		t.Fatal("abort released foreground budget", grants)
	}
	if _, err := s.PrepareDetach(shell.ID, shell.Owner, "control-2"); err != nil {
		t.Fatal(err)
	}
	committed, grants, err := s.CommitDetach(shell.ID, shell.Owner, "control-2", now)
	if err != nil || !committed.Background || committed.DetachRequest != "" || len(grants) != 1 || grants[0].ID != sibling.ID {
		t.Fatal(committed, grants, err)
	}
	if _, grants, err := s.CommitDetach(shell.ID, shell.Owner, "control-2", now); err != nil || len(grants) != 0 {
		t.Fatal("duplicate commit", grants, err)
	}
	if err := s.AbortDetach(shell.ID, shell.Owner, "control-2"); err == nil {
		t.Fatal("committed ownership silently reverted")
	}
}
