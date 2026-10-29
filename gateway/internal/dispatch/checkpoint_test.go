package dispatch

import (
	"reflect"
	"testing"
	"time"
)

func jobRequest() Request {
	req := request("loldboot-1", "source")
	req.Owner = "node/oldboot"
	req.JobID = "j_oldboot-1"
	req.Name = "Shell"
	req.Agent = "agent"
	req.Exclusive = []string{"dir:/project"}
	req.Shell = true
	return req
}
func TestJobSyncCompletesOrPreservesPreparedHandoff(t *testing.T) {
	now := time.Now()
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "saving"}[pending], func(t *testing.T) {
			s, _ := New(Defaults())
			req := jobRequest()
			s.Admit(req, now)
			if _, err := s.PrepareDetach(req.ID, req.Owner, req.JobID); err != nil {
				t.Fatal(err)
			}
			checkpoint := s.Checkpoint()
			restored, _ := New(Defaults())
			if err := restored.Restore(checkpoint, true); err != nil {
				t.Fatal(err)
			}
			observed := checkpoint.Leases[0]
			observed.State = "running"
			observed.Background = !pending
			if !pending {
				observed.DetachRequest = ""
			}
			if _, err := restored.SyncNode("node", req.Owner, []Lease{observed}, now); err != nil {
				t.Fatal(err)
			}
			got, _ := restored.Lookup(req.ID)
			if got.Background == pending || pending && got.DetachRequest != req.JobID || !pending && got.DetachRequest != "" {
				t.Fatal(got)
			}
			// Cancellation after a reserved node handoff cannot prevent its
			// ownership commit; physical resource release still waits for exit.
			if _, err := restored.Cancel(req.ID, req.Owner, now); err != nil {
				t.Fatal(err)
			}
			got, _, err := restored.CommitDetach(req.ID, req.Owner, req.JobID, now)
			if err != nil || !got.Background || got.State != "cancelling" {
				t.Fatal(got, err)
			}
			competitor := request("other", "other")
			competitor.Shared = []string{"dir:/project/sub"}
			queued, grants, err := restored.Admit(competitor, now)
			if err != nil || queued.State != "queued" || len(grants) != 0 {
				t.Fatal(queued, grants, err)
			}
		})
	}
}
func TestJobSyncMissingAndRebootUnknownRetainResourcesUntilReceipt(t *testing.T) {
	now := time.Now()
	s, _ := New(Defaults())
	req := jobRequest()
	req.Background = true
	if _, _, err := s.Admit(req, now); err != nil {
		t.Fatal(err)
	}
	// An incomplete inventory cannot retire a durable Job reservation.
	if _, err := s.SyncNode("node", "node/newboot", nil, now); err != nil {
		t.Fatal(err)
	}
	held, _ := s.Lookup(req.ID)
	if held.State != "unknown" || !held.Background || held.Owner != req.Owner {
		t.Fatal(held)
	}
	competitor := request("competing", "other")
	competitor.Owner = "other/boot"
	competitor.Shared = []string{"dir:/project/sub"}
	queued, _, _ := s.Admit(competitor, now)
	if queued.State != "queued" {
		t.Fatal(queued)
	}
	observed := held
	observed.Owner = "node/newboot"
	if _, err := s.SyncNode("node", observed.Owner, []Lease{observed}, now); err != nil {
		t.Fatal(err)
	}
	rebound, _ := s.Lookup(req.ID)
	if rebound.Owner != observed.Owner || rebound.State != "unknown" || !rebound.Background {
		t.Fatal(rebound)
	}
	// Unknown is a terminal notification, never evidence the process is gone.
	bad := observed
	bad.TerminalReceipt = true
	before := s.Checkpoint()
	if _, err := s.SyncNode("node", observed.Owner, []Lease{bad}, now); err == nil {
		t.Fatal("unknown receipt released resources")
	}
	if !reflect.DeepEqual(before, s.Checkpoint()) {
		t.Fatal("failed reconciliation changed state")
	}
	receipt := observed
	receipt.TerminalReceipt = true
	receipt.State = "cancelled"
	grants, err := s.SyncNode("node", observed.Owner, []Lease{receipt}, now)
	if err != nil || len(grants) != 1 || grants[0].ID != competitor.ID {
		t.Fatal(grants, err)
	}
	if _, ok := s.Lookup(req.ID); ok {
		t.Fatal("definitive receipt did not close job")
	}
	if grants, err = s.SyncNode("node", observed.Owner, []Lease{receipt}, now); err != nil || len(grants) != 0 {
		t.Fatal("duplicate receipt", grants, err)
	}
	if _, err := s.SyncNode("node", observed.Owner, []Lease{observed}, now); err == nil {
		t.Fatal("old active inventory resurrected a closed Job")
	}
}
func TestJobSyncValidatesAllImmutableIdentityBeforeMutation(t *testing.T) {
	now := time.Now()
	for _, field := range []string{"job", "call", "scope", "owner", "background", "pending", "creator", "silent"} {
		t.Run(field, func(t *testing.T) {
			s, _ := New(Defaults())
			req := jobRequest()
			s.Admit(req, now)
			original, _ := s.Lookup(req.ID)
			observed := original
			observed.Owner = "node/newboot"
			observed.State = "unknown"
			switch field {
			case "creator":
				observed.Creator = "another"
			case "silent":
				observed.Silent = !observed.Silent
			case "job":
				observed.JobID = "j_oldboot-foreign"
			case "call":
				observed.CallID = "another"
			case "scope":
				observed.Exclusive = []string{"dir:/project/sub"}
			case "owner":
				observed.Owner = "foreign/boot"
			case "background":
				observed.Background = true
			case "pending":
				observed.DetachRequest = observed.JobID
			}
			before := s.Checkpoint()
			if _, err := s.SyncNode("node", "node/newboot", []Lease{observed}, now); err == nil {
				t.Fatal("changed immutable execution accepted")
			}
			if !reflect.DeepEqual(before, s.Checkpoint()) {
				t.Fatal("rejected plan changed reservations")
			}
		})
	}
}
func TestJobForegroundSyncAbortsUnusedReservation(t *testing.T) {
	now := time.Now()
	s, _ := New(Defaults())
	req := jobRequest()
	s.Admit(req, now)
	s.PrepareDetach(req.ID, req.Owner, req.JobID)
	observed, _ := s.Lookup(req.ID)
	observed.DetachRequest = ""
	if _, err := s.SyncNode("node", req.Owner, []Lease{observed}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CommitDetach(req.ID, req.Owner, req.JobID, now); err == nil {
		t.Fatal("unused prepared capacity activated old ownership")
	}
}
