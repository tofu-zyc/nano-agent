package config

import (
	"fmt"
	"nano-agent/gateway/internal/dispatch"
	"time"
)

// Runtime is frozen at Gateway startup. Changing these settings requires a
// restart, keeping the C policy and durable machine arbiter on one set of limits.
type Runtime struct {
	LongCallHintSeconds         int `toml:"long_call_hint_seconds" json:"long_call_hint_seconds"`
	MaxCallsPerTurn             int `toml:"max_calls_per_turn" json:"max_calls_per_turn"`
	MaxActiveRunsPerNode        int `toml:"max_active_runs_per_node" json:"max_active_runs_per_node"`
	MaxParallelToolsPerRun      int `toml:"max_parallel_tools_per_run" json:"max_parallel_tools_per_run"`
	MaxActiveExecsPerMachine    int `toml:"max_active_execs_per_machine" json:"max_active_execs_per_machine"`
	MaxParallelShellsPerMachine int `toml:"max_parallel_shells_per_machine" json:"max_parallel_shells_per_machine"`
	MaxBackgroundJobsPerAgent   int `toml:"max_background_jobs_per_agent" json:"max_background_jobs_per_agent"`
	MaxLiveJobsPerMachine       int `toml:"max_live_jobs_per_machine" json:"max_live_jobs_per_machine"`
	MaxQueuedCallsPerRun        int `toml:"max_queued_calls_per_run" json:"max_queued_calls_per_run"`
}

func (r Runtime) Defaults() Runtime {
	if r.LongCallHintSeconds == 0 {
		r.LongCallHintSeconds = 10
	}
	if r.MaxCallsPerTurn == 0 {
		r.MaxCallsPerTurn = 32
	}
	if r.MaxActiveRunsPerNode == 0 {
		r.MaxActiveRunsPerNode = 16
	}
	if r.MaxParallelToolsPerRun == 0 {
		r.MaxParallelToolsPerRun = 4
	}
	if r.MaxActiveExecsPerMachine == 0 {
		r.MaxActiveExecsPerMachine = 8
	}
	if r.MaxParallelShellsPerMachine == 0 {
		r.MaxParallelShellsPerMachine = 2
	}
	if r.MaxBackgroundJobsPerAgent == 0 {
		r.MaxBackgroundJobsPerAgent = 4
	}
	if r.MaxLiveJobsPerMachine == 0 {
		r.MaxLiveJobsPerMachine = 16
	}
	if r.MaxQueuedCallsPerRun == 0 {
		r.MaxQueuedCallsPerRun = 32
	}
	return r
}
func (r Runtime) Validate() error {
	r = r.Defaults()
	if r.LongCallHintSeconds < 1 || r.LongCallHintSeconds > 3600 {
		return fmt.Errorf("runtime.long_call_hint_seconds 需在 1–3600 之间")
	}
	if r.MaxCallsPerTurn < 1 || r.MaxCallsPerTurn > 32 {
		return fmt.Errorf("runtime.max_calls_per_turn 需在 1–32 之间")
	}
	if r.MaxActiveRunsPerNode < 1 || r.MaxActiveRunsPerNode > 256 {
		return fmt.Errorf("runtime.max_active_runs_per_node 需在 1–256 之间")
	}
	if r.MaxParallelToolsPerRun < 1 || r.MaxParallelToolsPerRun > 32 {
		return fmt.Errorf("runtime.max_parallel_tools_per_run 需在 1–32 之间")
	}
	if r.MaxActiveExecsPerMachine < 1 || r.MaxActiveExecsPerMachine > 64 {
		return fmt.Errorf("runtime.max_active_execs_per_machine 需在 1–64 之间")
	}
	if r.MaxParallelShellsPerMachine < 1 || r.MaxParallelShellsPerMachine > 64 {
		return fmt.Errorf("runtime.max_parallel_shells_per_machine 需在 1–64 之间")
	}
	if r.MaxBackgroundJobsPerAgent < 1 || r.MaxBackgroundJobsPerAgent > 256 {
		return fmt.Errorf("runtime.max_background_jobs_per_agent 需在 1–256 之间")
	}
	if r.MaxLiveJobsPerMachine < 1 || r.MaxLiveJobsPerMachine > 256 {
		return fmt.Errorf("runtime.max_live_jobs_per_machine 需在 1–256 之间")
	}
	if r.MaxQueuedCallsPerRun < 1 || r.MaxQueuedCallsPerRun > 256 {
		return fmt.Errorf("runtime.max_queued_calls_per_run 需在 1–256 之间")
	}
	if r.MaxActiveExecsPerMachine < 2 || r.MaxLiveJobsPerMachine < r.MaxActiveExecsPerMachine || r.MaxParallelShellsPerMachine > r.MaxActiveExecsPerMachine {
		return fmt.Errorf("runtime：机器槽至少 2，未结束任务上限不小于槽数，命令并行数不大于槽数")
	}
	return nil
}
func (r Runtime) Dispatch() dispatch.Config {
	r = r.Defaults()
	return dispatch.Config{Slots: r.MaxActiveExecsPerMachine, Shells: r.MaxParallelShellsPerMachine,
		Live: r.MaxLiveJobsPerMachine, PerRun: r.MaxParallelToolsPerRun,
		QueuedPerRun: r.MaxQueuedCallsPerRun, BackgroundPerAgent: r.MaxBackgroundJobsPerAgent,
		HookReserve: 1, Total: 1024, AgeStep: time.Second, QueueWait: 5 * time.Minute}
}
