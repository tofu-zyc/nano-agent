// Package commit 是承诺的共同语言（设计 §15）：一份工作契约和一个从事实推出阶段的纯函数。
// 约定、回访、定时任务、Hook、分支各是自己的对象，保留自己的到期、补跑、取消和验收规则；
// 这里只管它们“是什么契约”和“现在处在哪个阶段”。不依赖其他包。
package commit

// 承诺的种类。
const (
	KindAgenda    = "agenda"    // 议程项
	KindIntention = "intention" // 意图（只列没有未完成议程项的）
	KindFollowUp  = "followup"  // 约好过会儿再看
	KindSchedule  = "schedule"  // 定时任务
	KindHook      = "hook"      // Hook 规则（不是单次投递）
	KindBranch    = "branch"    // 分支
	KindTicket    = "ticket"    // 工单（团队，设计 §16）
)

// Kinds 是固定顺序的七种。
var Kinds = []string{KindAgenda, KindIntention, KindFollowUp, KindSchedule, KindHook, KindBranch, KindTicket}

// 阶段：前五个是进行中，后四个是结束，对应设计第 4 节的四种结束。
const (
	Waiting = "waiting" // 等待：没到时间、在等事件，或到期了但还不能开始
	Running = "running" // 进行中：有一轮正在跑
	Blocked = "blocked" // 受阻：要人处理
	Ready   = "ready"   // 就绪：有结果等人看，还没看过
	Paused  = "paused"  // 暂停：人停下的，可以恢复
	Done    = "done"    // 完成：有证据
	Dropped = "dropped" // 无需处理：有理由
	Failed  = "failed"  // 失败：不再重试
	Stopped = "stopped" // 叫停：人取消或归档的
)

// Phases 是全部九个阶段（固定顺序）。
var Phases = []string{Waiting, Running, Blocked, Ready, Paused, Done, Dropped, Failed, Stopped}

// IsEnded 是结束阶段：完成、无需处理、失败、叫停。
func IsEnded(phase string) bool {
	return phase == Done || phase == Dropped || phase == Failed || phase == Stopped
}

// Contract 是一件承诺的工作契约（设计第 4 节的五项）：从各自的字段推出来，不另存一份。
type Contract struct {
	Goal     string   `json:"goal"`      // 目标
	By       string   `json:"by"`        // 授权：谁让做的
	Scene    string   `json:"scene"`     // 授权：在哪个会话或场景
	Scope    string   `json:"scope"`     // 授权：能动到哪里（权限模式、工作目录、工具），一句话
	Done     string   `json:"done"`      // 完成标准；没写就是空
	ReportTo []string `json:"report_to"` // 汇报去处；空表示只留在自己的记录里
	Stop     string   `json:"stop"`      // 停止条件：预算、时限、次数，一句话
}

// Facts 是从各种承诺自己的记录推出来的事实。
type Facts struct {
	Now      int64
	Ended    string // 自己的记录里已经结束：done | dropped | failed | stopped
	Evidence string // done 的证据
	Reason   string // dropped、failed、stopped 的理由
	Paused   bool
	Running  bool
	Question string // 还没回答的问题
	Blocked  string // 受阻的原因
	Ready    string // 等人看的结果
	Mark     string // Blocked、Ready、Question 来自哪一条事实；“知道了”和回答都靠它
	Due      int64  // 下一次到期；0 表示在等事件
	Wait     string // 到期了还不能开始的原因
}

// Phase 从事实推出阶段和一句原因。从上往下，第一行命中就是结果：
//
//	 1  Ended 是 done，Evidence 空        → blocked  说做完了，但没有证据
//	 2  Ended 是 dropped，Reason 空       → blocked  说不用做了，但没写理由
//	 3  Ended 不空                        → 同 Ended，原因是 Evidence 或 Reason
//	 4  Paused                            → paused
//	 5  Running                           → running
//	 6  Question 不空                     → blocked  等你回答：Question
//	 7  Blocked 不空                      → blocked  Blocked
//	 8  Ready 不空                        → ready    Ready
//	 9  Due > Now                         → waiting  等到某个时间
//	10  Due > 0，Wait 不空                → waiting  Wait
//	11  Due > 0                           → waiting  到期了，马上开始
//	12  其余                              → waiting  等事件
//
// 第 1、2 行推广到 failed 和 stopped：没有理由的结束一样算受阻，也不认识的结束值同理，
// 这样“结束阶段一定带着证据或理由”对任何一组事实都成立。
func Phase(f Facts) (phase, why string) {
	switch f.Ended {
	case "":
	case Done:
		if f.Evidence == "" {
			return Blocked, "说做完了，但没有证据"
		}
		return Done, f.Evidence
	case Dropped:
		if f.Reason == "" {
			return Blocked, "说不用做了，但没写理由"
		}
		return Dropped, f.Reason
	case Failed, Stopped:
		if f.Reason == "" {
			if f.Ended == Failed {
				return Blocked, "说失败了，但没写原因"
			}
			return Blocked, "说叫停了，但没写理由"
		}
		return f.Ended, f.Reason
	default:
		return Blocked, "不认识的结束状态：" + f.Ended
	}
	switch {
	case f.Paused:
		return Paused, ""
	case f.Running:
		return Running, ""
	case f.Question != "":
		return Blocked, "等你回答：" + f.Question
	case f.Blocked != "":
		return Blocked, f.Blocked
	case f.Ready != "":
		return Ready, f.Ready
	case f.Due > f.Now:
		return Waiting, "等到某个时间"
	case f.Due > 0 && f.Wait != "":
		return Waiting, f.Wait
	case f.Due > 0:
		return Waiting, "到期了，马上开始"
	}
	return Waiting, "等事件"
}
