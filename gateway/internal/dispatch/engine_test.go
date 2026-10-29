package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func next(t *testing.T, ch <-chan Change) Change {
	t.Helper()
	select {
	case change := <-ch:
		return change
	case <-time.After(3 * time.Second):
		t.Fatal("lease persistence did not complete")
		return Change{}
	}
}
func submit(t *testing.T, e *Engine, command Command) {
	t.Helper()
	if !e.Submit(command) {
		t.Fatal("lease command was not accepted")
	}
}

func TestDurableGrantUnknownRecoveryAndInventoryNoReplay(t *testing.T) {
	file := filepath.Join(t.TempDir(), "leases.json")
	changes := make(chan Change, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, err := Open(ctx, file, Defaults(), func(c Change) { changes <- c })
	if err != nil {
		t.Fatal(err)
	}
	req := request("lboot-1", "sid")
	req.Owner = "node/boot"
	req.Exclusive = []string{"dir:/project"}
	submit(t, e, Command{Action: "admit", Request: req})
	change := next(t, changes)
	if change.Err != nil || len(change.Grants) != 1 {
		t.Fatal(change)
	}
	bytes, err := os.ReadFile(file)
	if err != nil || len(bytes) == 0 {
		t.Fatal("grant published before durable record", err)
	}
	cancel()
	<-e.Done()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	restored, err := Open(ctx2, file, Defaults(), func(c Change) { changes <- c })
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := restored.Snapshot()
	if len(checkpoint.Leases) != 1 || checkpoint.Leases[0].State != "unknown" {
		t.Fatal("restart forgot real process reservation", checkpoint)
	}
	select {
	case c := <-changes:
		t.Fatal("recovery replayed an old command", c)
	default:
	}
	competing := request("lother-1", "other")
	competing.Owner = "other/boot"
	competing.Shared = []string{"dir:/project/sub"}
	submit(t, restored, Command{Action: "admit", Request: competing})
	change = next(t, changes)
	if change.Lease.State != "queued" || len(change.Grants) != 0 {
		t.Fatal("unknown reservation released early", change)
	}
	// Authenticated boot change with a complete empty execution inventory is
	// evidence that this node has no process to preserve. It never replays req.
	submit(t, restored, Command{Action: "sync", Node: "node", Owner: "node/new-boot"})
	change = next(t, changes)
	if change.Err != nil || len(change.Grants) != 1 || change.Grants[0].ID != competing.ID {
		t.Fatal(change)
	}
}

func TestPersistenceFailureCannotPublishOrActivateUncommittedGrant(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "leases.json")
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Create engine before replacing its target with an unwritable publication
	// point; an existing directory at Open would correctly be a recovery error.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	changes := make(chan Change, 16)
	e, err := Open(ctx, file, Defaults(), func(c Change) { changes <- c })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	submit(t, e, Command{Action: "admit", Request: request("failed", "sid")})
	change := next(t, changes)
	if change.Err == nil || len(change.Grants) != 0 {
		t.Fatal("uncommitted grant published", change)
	}
	checkpoint, storageErr := e.Snapshot()
	if len(checkpoint.Leases) != 0 || storageErr == "" {
		t.Fatal("failed mutation activated", checkpoint, storageErr)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	submit(t, e, Command{Action: "admit", Request: request("later", "sid")})
	change = next(t, changes)
	if change.Err != nil || len(change.Grants) != 1 {
		t.Fatal(change)
	}
}

func TestCorruptLeaseCheckpointIsVisibleAndRetained(t *testing.T) {
	file := filepath.Join(t.TempDir(), "leases.json")
	if err := os.WriteFile(file, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), file, Defaults(), nil); err == nil {
		t.Fatal("corrupt ownership discarded")
	}
	if bytes, _ := os.ReadFile(file); string(bytes) != "broken" {
		t.Fatal("recovery rewrote corrupt source")
	}
}

func TestDetachPrepareIsDurableAndCommitWaitsForMatchingOwner(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan Change, 16)
	e, err := Open(ctx, filepath.Join(root, "leases.json"), Defaults(), func(c Change) { changes <- c })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-e.Done() })
	req := request("job-lease", "source")
	submit(t, e, Command{Action: "admit", Request: req})
	next(t, changes)
	submit(t, e, Command{Action: "detach_prepare", ID: req.ID, Owner: req.Owner, RequestID: "ctl"})
	prepared := next(t, changes)
	if prepared.Err != nil || prepared.RequestID != "ctl" || prepared.Lease.Background || prepared.Lease.DetachRequest != "ctl" {
		t.Fatal(prepared)
	}
	durable, err := os.ReadFile(filepath.Join(root, "leases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint Checkpoint
	if err := json.Unmarshal(durable, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Leases) != 1 || checkpoint.Leases[0].DetachRequest != "ctl" || checkpoint.Leases[0].Background {
		t.Fatal(checkpoint)
	}
	submit(t, e, Command{Action: "detach_commit", ID: req.ID, Owner: "foreign", RequestID: "ctl"})
	rejected := next(t, changes)
	if rejected.Err == nil {
		t.Fatal("foreign execution owner committed")
	}
	submit(t, e, Command{Action: "detach_commit", ID: req.ID, Owner: req.Owner, RequestID: "ctl"})
	committed := next(t, changes)
	if committed.Err != nil || !committed.Lease.Background {
		t.Fatal(committed)
	}
}
