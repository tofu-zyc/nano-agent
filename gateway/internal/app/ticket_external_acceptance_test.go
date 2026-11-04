package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nano-agent/gateway/internal/extagent"
	"nano-agent/gateway/internal/llm"
	"nano-agent/gateway/internal/ticket"
)

// 5d-4：设计 16.4 验收清单里还要真内核走一遍的几条（替身脚本当外部 agent）。清单的其余几条在
// external_tool_test.go、ticket_external_test.go、ticket_jobs_test.go、external_realkernel_test.go、
// ticket_external_realkernel_test.go 和内核的 test_exec.py、test_host.py 里。

// replyStep 是替身模型这一轮看到的：最后一条用户消息和这一轮已经有结果的调用。
type replyStep struct {
	text    string
	done    map[string]bool
	content map[string]string
}

func (s replyStep) call(id, name, args string) (*llm.Result, error) {
	return &llm.Result{Reason: llm.ReasonToolUse, Calls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}}}, nil
}

func (s replyStep) final(text string) (*llm.Result, error) {
	return &llm.Result{Reason: llm.ReasonFinal, Content: text}, nil
}

type replyScript func(s replyStep) (*llm.Result, error)

func (f replyScript) Chat(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Result, error) {
	s := replyStep{done: map[string]bool{}, content: map[string]string{}}
	last := -1
	for i, m := range req.Messages {
		if m.Role == "user" {
			last, s.done, s.content = i, map[string]bool{}, map[string]string{}
		}
		if m.Role == "tool" {
			s.done[m.ToolCallID], s.content[m.ToolCallID] = true, m.Content
		}
	}
	if last >= 0 {
		s.text = req.Messages[last].Content
	}
	return f(s)
}

// 同一会话里的普通 Shell 读不到外部 agent 的登录目录，也读不到它往私有 HOME 里写的东西（真内核、真执行器）。
func TestRealKernelOrdinaryShellCannotSeeTheExternalAgentsFiles(t *testing.T) {
	var x *extTicketEnv
	model := replyScript(func(s replyStep) (*llm.Result, error) {
		switch {
		case strings.Contains(s.text, "[工单 tk-1"):
			if !s.done["ext"] {
				return s.call("ext", "External", `{"name":"fake","prompt":"登录一下"}`)
			}
			if !s.done["wait"] {
				at := strings.Index(s.content["ext"], "job_id")
				id := s.content["ext"][at+len(`job_id":"`):]
				id = id[:strings.Index(id, `"`)]
				return s.call("wait", "Job", `{"action":"wait","job_id":"`+id+`","wait_ms":10000}`)
			}
			if !s.done["sh"] { // 普通 Shell：读登记的目录、读它的私有 HOME
				cmd := "cat " + filepath.Join(x.agentDir, "cred") + " 2>&1; echo ---; cat " + filepath.Join(x.home, ".nano-agent", "tmp", "ext-fake-*", "home", "private.txt") + " 2>&1; echo ---; echo mine > \"$HOME/mine.txt\" && echo shell-home-ok"
				return s.call("sh", "Shell", `{"command":`+quoteJSON(cmd)+`}`)
			}
			return s.final("看过了")
		case strings.Contains(s.text, "修登录页"):
			if !s.done["tc"] {
				return s.call("tc", "Ticket", `{"action":"create","title":"修登录页","goal":"x"}`)
			}
		}
		return s.final("嗯")
	})
	x = newExtTicketEnv(t, []string{"~/.fake-agent"}, `echo secret-login > "$FAKE_AGENT_DIR/cred"; echo private-note > "$HOME/private.txt"`, model)
	x.startTeam(t, "true")
	mention(x.env, "@机器人 修登录页", "g100", "u2", "李四")
	eventually(t, "分支的 Shell 跑完", func() bool {
		tk := ticketNow(x.env, "tk-1")
		return tk.RunSID != "" && x.branchResults(tk.RunSID)["sh"] != ""
	})
	// 外部 agent 真的写了：登记的目录里有登录凭据，私有 HOME 里有它的东西（从沙箱外看得到）
	if b, _ := os.ReadFile(filepath.Join(x.agentDir, "cred")); strings.TrimSpace(string(b)) != "secret-login" {
		t.Fatalf("外部 agent 能写登记的目录：%q", b)
	}
	priv, _ := filepath.Glob(filepath.Join(x.home, ".nano-agent", "tmp", "ext-fake-*", "home", "private.txt"))
	if len(priv) != 1 {
		t.Fatalf("外部 agent 的私有 HOME 里有它写的东西：%v", priv)
	}
	tk := ticketNow(x.env, "tk-1")
	out := x.branchResults(tk.RunSID)["sh"]
	if strings.Contains(out, "secret-login") || strings.Contains(out, "private-note") {
		t.Fatalf("普通 Shell 读到了外部 agent 的东西：%s", out)
	}
	if strings.Count(out, "Permission denied")+strings.Count(out, "No such file") < 2 {
		t.Fatalf("读登记的目录、读私有 HOME 都该被拒（读不到）：%s", out)
	}
	if !strings.Contains(out, "shell-home-ok") {
		t.Fatalf("普通 Shell 有自己的私有 HOME：%s", out)
	}
}

func quoteJSON(s string) string {
	b := new(strings.Builder)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// 登记的目录不合规：执行被拒，Job 工具的 wait 看得到原因，外部 agent 一行都没跑（真内核、真执行器）。
// 家目录本身在登记时就拒绝了（不含 ~/ 本身）；PATH 里的目录、符号链接、沙箱关着的各种写法在执行器的测试里逐项试过。
func TestRealKernelExternalDirsThatAreNotAllowedAreRefused(t *testing.T) {
	cases := []struct{ dir, want string }{
		{"~/.nano-agent", "NANO_HOME"},    // 在 NANO_HOME 里（这里就是它）
		{"~/proj", "工作目录互相包含"},            // 包着仓库
		{"~/proj/repo/.git", "只读目录互相包含"},  // 在 .git 里（只读）
		{"~/proj/repo/src", "仓库的根"},       // 在工单的主仓库里
		{"~/proj/repo/.nano", "工作目录互相包含"}, // 包着 worktree
		{"~/nope", "目录不存在"},               // 不存在
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			x := newExtTicketEnv(t, []string{"~/.fake-agent", c.dir}, "", ticketExtModel{agent: "fake"})
			if err := os.MkdirAll(filepath.Join(x.home, ".nano-agent"), 0o755); err != nil { // NANO_HOME 要存在，才是“和它互相包含”被拒
				t.Fatal(err)
			}
			x.startTeam(t, "true")
			mention(x.env, "@机器人 修登录页", "g100", "u2", "李四")
			eventually(t, "分支等完 Job", func() bool {
				tk := ticketNow(x.env, "tk-1")
				return tk.RunSID != "" && strings.Contains(x.branchResults(tk.RunSID)["wait0"], "job_id")
			})
			tk := ticketNow(x.env, "tk-1")
			wait := x.branchResults(tk.RunSID)["wait0"]
			if !strings.Contains(wait, `"status":"failed"`) || !strings.Contains(wait, c.want) || !strings.Contains(wait, "没有执行") {
				t.Fatalf("被执行器拒绝，Job 工具看得到原因（%q）：%s", c.want, wait)
			}
			for _, f := range []string{filepath.Join(x.repo, ".nano", "worktrees", "tk-1", "ext-prompt.txt"), filepath.Join(x.agentDir, "last-run")} {
				if _, err := os.Stat(f); err == nil {
					t.Fatalf("被拒绝的不该跑：%s", f)
				}
			}
		})
	}
	// 登记时就拒绝：家目录本身、含 ..
	x := newExtTicketEnv(t, []string{"~/.fake-agent"}, "", ticketExtModel{agent: "fake"})
	for _, bad := range []string{"~/", "~/proj/../.ssh"} {
		ag := extagent.Agent{Name: "other", Argv: []string{"sh", "-c", "true", "x", extagent.PromptSlot}, Dirs: []string{bad}}
		if _, err := x.a.SaveExternalAgent(ag); err == nil {
			t.Fatalf("登记时就该拒绝：%s", bad)
		}
	}
}

// 分支 Report ready 时外部 agent 还在跑：被拒，写明是哪个 Job；这一轮结束不算没汇报；叫停工单时这个 Job 被停掉，
// 进程真的没了，worktree 清理掉（真内核、真执行器）。
func TestRealKernelLiveExternalJobBlocksReadyAndCancelStopsIt(t *testing.T) {
	model := replyScript(func(s replyStep) (*llm.Result, error) {
		switch {
		case strings.Contains(s.text, "[工单 tk-1"):
			if !s.done["w1"] { // 先写（凭据）：外部 agent 跑起来之后它占着 worktree，分支自己的 Write 要排队
				return s.call("w1", "Write", `{"path":"note.txt","content":"x\n"}`)
			}
			if !s.done["ext"] {
				return s.call("ext", "External", `{"name":"fake","prompt":"慢慢做"}`)
			}
			if !s.done["r1"] {
				return s.call("r1", "Report", `{"status":"ready","text":"改好了","evidence":["w1"]}`)
			}
			return s.final("等它结束")
		case strings.Contains(s.text, "叫停"):
			if !s.done["cx"] {
				return s.call("cx", "Ticket", `{"action":"cancel","id":"tk-1","text":"不要了"}`)
			}
			return s.final("叫停了")
		case strings.Contains(s.text, "修登录页"):
			if !s.done["tc"] {
				return s.call("tc", "Ticket", `{"action":"create","title":"修登录页","goal":"x"}`)
			}
		}
		return s.final("嗯")
	})
	t.Cleanup(func() { _ = exec.Command("pkill", "-f", "[s]leep 4747").Run() }) // 失败时别留下一个睡着的进程
	x := newExtTicketEnv(t, []string{"~/.fake-agent"}, `touch started; sleep 4747`, model)
	x.startTeam(t, "true")
	mention(x.env, "@机器人 修登录页", "g100", "u2", "李四")
	eventually(t, "分支这一轮结束，在等 Job", func() bool {
		tk := ticketNow(x.env, "tk-1")
		return len(tk.WaitJobs) == 1 && x.branchResults(tk.RunSID)["r1"] != ""
	})
	tk := ticketNow(x.env, "tk-1")
	r1 := x.branchResults(tk.RunSID)["r1"]
	if !strings.Contains(r1, "没结束的后台任务") || !strings.Contains(r1, tk.WaitJobs[0]) {
		t.Fatalf("Report ready 被拒，写明是哪个 Job：%s", r1)
	}
	if tk.Status != ticket.StatusWorking || tk.WantCheck {
		t.Fatalf("不算没汇报，也没记要验收：%+v", tk)
	}
	eventually(t, "外部 agent 的进程起来了", func() bool { out, _ := exec.Command("pgrep", "-f", "[s]leep 4747").Output(); return len(out) > 0 })
	// 叫停：Job 被停掉，进程没了
	mention(x.env, "@机器人 叫停吧", "g200", "u2", "李四")
	eventually(t, "工单叫停", func() bool { return ticketNow(x.env, "tk-1").Status == ticket.StatusStopped })
	eventually(t, "Job 被停掉", func() bool {
		x.a.mu.Lock()
		defer x.a.mu.Unlock()
		jobs := x.a.jobEvents.JobsBySession(tk.RunSID)
		return len(jobs) == 1 && finalJob(jobs[0].Receipt.Status)
	})
	x.a.mu.Lock()
	status := x.a.jobEvents.JobsBySession(tk.RunSID)[0].Receipt.Status
	x.a.mu.Unlock()
	if status != "cancelled" {
		t.Fatalf("Job 是被取消的：%s", status)
	}
	eventually(t, "进程没了", func() bool { out, _ := exec.Command("pgrep", "-f", "[s]leep 4747").Output(); return len(out) == 0 })
	eventually(t, "worktree 清掉", func() bool {
		_, err := os.Stat(filepath.Join(x.repo, ".nano", "worktrees", "tk-1"))
		return os.IsNotExist(err)
	})
}

// 一轮结束时外部 agent 还在跑：不算没汇报，工单一直 working；Job 结束叫醒分支一次，分支接着汇报、验收通过（真内核、真执行器）。
func TestRealKernelExternalJobEndWakesTheBranchOnce(t *testing.T) {
	model := replyScript(func(s replyStep) (*llm.Result, error) {
		switch {
		case strings.Contains(s.text, "[工单 tk-1"):
			if !s.done["w1"] { // 先写（凭据）：外部 agent 跑起来之后它占着 worktree，分支自己的 Write 要排队
				return s.call("w1", "Write", `{"path":"note.txt","content":"x\n"}`)
			}
			if !s.done["ext"] {
				return s.call("ext", "External", `{"name":"fake","prompt":"改好 a.txt"}`)
			}
			if !s.done["r1"] {
				return s.call("r1", "Report", `{"status":"ready","text":"改好了","evidence":["w1"]}`) // 被拒：agent 还在跑
			}
			return s.final("等它结束")
		case strings.Contains(s.text, "[后台任务]"):
			if !s.done["w2"] {
				return s.call("w2", "Write", `{"path":"note2.txt","content":"y\n"}`)
			}
			if !s.done["r2"] {
				return s.call("r2", "Report", `{"status":"ready","text":"外部 agent 改好了","evidence":["w2"]}`)
			}
			return s.final("做完了")
		case strings.Contains(s.text, "修登录页"):
			if !s.done["tc"] {
				return s.call("tc", "Ticket", `{"action":"create","title":"修登录页","goal":"x"}`)
			}
		}
		return s.final("嗯")
	})
	x := newExtTicketEnv(t, []string{"~/.fake-agent"}, `sleep 2; echo 二 >> a.txt`, model)
	x.startTeam(t, "grep -q 二 a.txt")
	mention(x.env, "@机器人 修登录页", "g100", "u2", "李四")
	eventually(t, "分支这一轮结束，在等 Job", func() bool {
		tk := ticketNow(x.env, "tk-1")
		return tk.RunSID != "" && x.branchResults(tk.RunSID)["r1"] != "" && (len(tk.WaitJobs) == 1 || len(tk.Woken) == 1 || tk.Status != ticket.StatusWorking)
	})
	eventually(t, "叫醒后验收通过、工单 ready", func() bool { return ticketNow(x.env, "tk-1").Status == ticket.StatusReady })
	tk := ticketNow(x.env, "tk-1")
	if len(tk.WaitJobs) != 0 || len(tk.Woken) != 1 || tk.Resumes != 0 || tk.Renewals != 0 {
		t.Fatalf("叫过一次，不算续上也不算被打断：%+v", tk)
	}
	x.a.mu.Lock()
	var hist strings.Builder
	for _, m := range x.a.store.History(tk.RunSID) {
		hist.WriteString(m.Content + "\n")
	}
	x.a.mu.Unlock()
	if n := strings.Count(hist.String(), "[后台任务]"); n != 1 || !strings.Contains(hist.String(), "（succeeded） 结束了") {
		t.Fatalf("只叫一次：%d\n%s", n, hist.String())
	}
	if got := gitRun(t, x.repo, "show", tk.Commit+":a.txt"); got != "一\n二" {
		t.Fatalf("验收的是外部 agent 改完的：%q", got)
	}
	// 再等一会儿：没有第二次叫醒
	time.Sleep(500 * time.Millisecond)
	if strings.Count(func() string { x.a.mu.Lock(); defer x.a.mu.Unlock(); return x.historyText(tk.RunSID) }(), "[后台任务]") != 1 {
		t.Fatal("每个 Job 只叫一次")
	}
}

func (x *extTicketEnv) historyText(sid string) string {
	var b strings.Builder
	for _, m := range x.a.store.History(sid) {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}
