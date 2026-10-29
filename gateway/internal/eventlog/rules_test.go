package eventlog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func ruleFixture() RuleRecord {
	return RuleRecord{ID: "finished", Revision: 1, AgentID: "agent", Creator: "fake:user", OperationID: "create-op",
		Enabled: true, Once: true, Body: json.RawMessage(`{"on":["device.online"],"handler":{"type":"notify","text":"done"}}`)}
}
func TestRulesAndOnceClaimAreAtomicAndSurvivePruningAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events")
	s := openTest(t, path, Limits{CompactBytes: 1})
	first := notice(1)
	commit(t, s, Transaction{Event: &first})
	r := ruleFixture()
	commit(t, s, Transaction{Rule: &RuleChange{Record: r}})
	if _, err := os.Stat(path + ".v1.bak"); err != nil {
		t.Fatal("v1 input was not retained", err)
	}
	d := delivery(first, r.ID)
	d.Rule = r.Body
	commit(t, s, Transaction{Match: &Match{EventID: first.ID, Checked: true, Deliveries: []Delivery{d}}})
	consumed, _ := s.Rule(r.ID)
	if consumed.ClaimedBy != first.ID || !consumed.Enabled {
		t.Fatal(consumed)
	}
	second := notice(2)
	commit(t, s, Transaction{Event: &second})
	duplicate := delivery(second, r.ID)
	duplicate.Rule = r.Body
	if err := s.Commit(context.Background(), Transaction{Match: &Match{EventID: second.ID, Checked: true, Deliveries: []Delivery{duplicate}}}); err == nil {
		t.Fatal("once claimed twice")
	}
	if e, _ := s.Event(second.ID); e.Matched {
		t.Fatal("failed claim partially published a match")
	}
	d, _ = s.Delivery(d.ID)
	if !d.Registered {
		t.Fatal("registered source flag lost")
	}
	d.Status = "running"
	d.Attempt = 1
	commit(t, s, Transaction{Delivery: &d})
	d.Status = "succeeded"
	commit(t, s, Transaction{Delivery: &d})
	consumed, _ = s.Rule(r.ID)
	if consumed.Enabled || consumed.ClaimedBy != first.ID {
		t.Fatal("successful once remained active", consumed)
	}
	commit(t, s, Transaction{Forget: []string{first.ID}})
	s.Close()
	s = openTest(t, path, Limits{CompactBytes: 1})
	recovered, _ := s.Rule(r.ID)
	if recovered.Enabled || recovered.ClaimedBy != first.ID {
		t.Fatal("pruning restored an old once rule", recovered)
	}
	if err := Verify(path); err != nil {
		t.Fatal(err)
	}
}
func TestRuleWithdrawalAndCASCannotBeReimportedAsAnotherIdentity(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "events"), Limits{})
	r := ruleFixture()
	commit(t, s, Transaction{Rule: &RuleChange{Record: r}})
	e := notice(1)
	commit(t, s, Transaction{Event: &e})
	d := delivery(e, r.ID)
	d.Rule = r.Body
	commit(t, s, Transaction{Match: &Match{EventID: e.ID, Checked: true, Deliveries: []Delivery{d}}})
	r.Revision++
	r.Enabled = false
	commit(t, s, Transaction{Rule: &RuleChange{Record: r, Expected: 1}})
	d, _ = s.Delivery(d.ID)
	d.Status = "running"
	d.Attempt = 1
	if err := s.Commit(context.Background(), Transaction{Delivery: &d}); err == nil {
		t.Fatal("withdrawn pending copy activated")
	}
	r.Revision++
	r.Deleted = true
	r.Body = json.RawMessage("{}")
	commit(t, s, Transaction{Rule: &RuleChange{Record: r, Expected: 2}})
	copied := ruleFixture()
	copied.ID = "different"
	if err := s.Commit(context.Background(), Transaction{Rule: &RuleChange{Record: copied}}); err == nil {
		t.Fatal("same operation recreated by changing ID")
	}
	r.Revision++
	r.Deleted = false
	r.Enabled = true
	if err := s.Commit(context.Background(), Transaction{Rule: &RuleChange{Record: r, Expected: 3}}); err == nil {
		t.Fatal("deleted ID was restored")
	}
	rows := s.Rules("")
	if len(rows) != 0 {
		t.Fatal("deleted rule visible as active", rows)
	}
}
