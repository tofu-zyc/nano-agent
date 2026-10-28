package config

import "testing"

func TestRuntimeSettingsAreBoundedAndRestarted(t *testing.T) {
	r := (Runtime{}).Defaults()
	if r.MaxCallsPerTurn != 32 || r.MaxActiveRunsPerNode != 16 || r.Dispatch().Slots != 8 || r.Dispatch().HookReserve != 1 {
		t.Fatal(r)
	}
	for _, bad := range []Runtime{{MaxCallsPerTurn: 33}, {MaxActiveRunsPerNode: 257}, {MaxParallelToolsPerRun: -1}, {MaxActiveExecsPerMachine: 1}, {MaxActiveExecsPerMachine: 20, MaxLiveJobsPerMachine: 16}} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid settings: %+v", bad)
		}
	}
	store, _, err := Open(writeConf(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(map[string]any{"runtime.max_parallel_tools_per_run": float64(2)}); err != nil {
		t.Fatal(err)
	}
	f := find(store.Describe(), "runtime.max_parallel_tools_per_run")
	if !f.Restart || !f.Pending {
		t.Fatalf("runtime policy changed without restart: %+v", f)
	}
	if err := store.Set(map[string]any{"runtime.max_calls_per_turn": float64(33)}); err == nil {
		t.Fatal("oversized proposal limit accepted")
	}
}
