package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"nano-agent/gateway/internal/proto"
)

func jobTool() proto.ToolSpec {
	return proto.ToolSpec{Name: "Job", Description: "查看真实执行节点上的后台/长时间 Shell 任务、读取有界日志、最多等待 10 秒、取消或把当前前台任务原地转后台。" +
		"list/status/wait 不启动命令；detach 不重启命令。取消回执仅表示受理，须以原任务终态核实。" +
		"backgrounded/queued/running/unknown 都不能当作工作完成。不需要立即知道结果时让后台任务继续，不要新增定时任务反复轮询。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
          "action":{"type":"string","enum":["list","status","output","wait","cancel","detach"]},
          "job_id":{"type":"string","maxLength":96},"node":{"type":"string","maxLength":128},
          "cursor":{"type":"integer","minimum":0,"maximum":9007199254740991},
          "max_bytes":{"type":"integer","minimum":1,"maximum":65536},
          "wait_ms":{"type":"integer","minimum":1,"maximum":10000},
          "limit":{"type":"integer","minimum":1,"maximum":100},"after":{"type":"string","maxLength":512}
        },"required":["action"]}`)}
}
func (a *App) jobToolCall(s *sess, m *proto.Msg, reply func(bool, string)) {
	var request JobRequest
	if err := json.Unmarshal(m.Args, &request); err != nil {
		reply(false, "Job 参数格式不正确")
		return
	}
	r := s.run
	if r.execs == nil {
		r.execs = map[string]context.CancelFunc{}
	}
	ctx, cancel := context.WithCancel(a.ctx)
	r.execs[m.ID] = cancel
	actor, sid := r.from, s.sid
	go func() {
		defer cancel()
		result, err := a.JobAction(ctx, actor, sid, request)
		a.mu.Lock()
		delete(r.execs, m.ID)
		a.mu.Unlock()
		if err != nil {
			reply(false, err.Error())
			return
		}
		raw, _ := json.Marshal(result)
		reply(true, string(raw))
	}()
}
func renderJob(j JobInfo) string {
	line := fmt.Sprintf("%s  %s  %s", j.JobID, j.Status, j.Node)
	if j.Background {
		line += " · 后台"
	}
	if j.Silent {
		line += " · 安静"
	}
	if !j.Online {
		line += " · 节点离线"
	}
	if j.Error != "" {
		line += "\n" + j.Error
	}
	return line
}
func renderJobResponse(out JobResponse) string {
	if out.Job == nil {
		if len(out.Jobs) == 0 {
			return "没有可查看的已登记任务"
		}
		lines := make([]string, 0, len(out.Jobs))
		for _, j := range out.Jobs {
			lines = append(lines, renderJob(j))
		}
		text := strings.Join(lines, "\n")
		if out.Next != "" {
			text += "\n还有更多任务，可在网页中继续查看。"
		}
		return text
	}
	text := renderJob(*out.Job)
	if out.Accepted && (out.Action == "cancel" || out.Action == "detach") {
		text += "\n操作已受理；以同一任务的后续状态为准。"
	}
	if out.WaitExpired {
		text += "\n短等待已结束，任务仍未取得终态。"
	}
	if out.Output != nil {
		var body strings.Builder
		for _, chunk := range out.Output.Chunks {
			data, err := base64.StdEncoding.Strict().DecodeString(chunk.Data)
			if err != nil {
				continue
			}
			body.WriteString(strings.ToValidUTF8(string(data), "�"))
		}
		text += "\n" + body.String() + fmt.Sprintf("\n下次游标：%d", out.Output.NextCursor)
		if out.Output.Gap {
			text += "（有日志缺口）"
		}
	}
	return text
}
func (a *App) cmdJobs(c in, command, arg string) {
	request := JobRequest{Action: "list"}
	fields := strings.Fields(arg)
	switch command {
	case "bg":
		request.Action = "detach"
		if len(fields) > 0 {
			request.JobID = fields[0]
		} else {
			if a.jobEvents == nil {
				a.answer(c, "可靠任务索引不可用")
				return
			}
			cursor := ""
			for {
				rows, next := a.jobEvents.Jobs(cursor, 100)
				for _, j := range rows {
					if j.Receipt.OriginSID == c.s.sid && !j.Receipt.Background && !finalJob(j.Receipt.Status) && a.canAccessJob(c.who, c.s.sid, j) {
						if request.JobID != "" {
							a.answer(c, "有多个前台任务，请用 /bg <job_id> 指定")
							return
						}
						request.JobID = j.Receipt.JobID
						request.Node = j.SourceID
					}
				}
				if next == "" {
					break
				}
				cursor = next
			}
			if request.JobID == "" {
				a.answer(c, "当前会话没有可转后台的已登记任务")
				return
			}
		}
	case "job":
		if len(fields) == 0 {
			a.answer(c, "用法：/job <job_id> [status|output [游标]|wait [秒]|cancel|detach]")
			return
		}
		request.JobID = fields[0]
		request.Action = "status"
		if len(fields) > 1 {
			request.Action = fields[1]
		}
		if request.Action == "stop" {
			request.Action = "cancel"
		}
		if len(fields) > 2 {
			if request.Action == "output" {
				n, err := strconv.ParseUint(fields[2], 10, 64)
				if err != nil {
					a.answer(c, "日志游标需为非负整数")
					return
				}
				request.Cursor = n
			} else if request.Action == "wait" {
				n, err := strconv.Atoi(fields[2])
				if err != nil || n < 1 || n > 10 {
					a.answer(c, "短等待为 1–10 秒")
					return
				}
				request.WaitMS = n * 1000
			}
		}
	}
	// User commands bypass the message/model queue and take no run budget.
	ctx, cancel := context.WithTimeout(a.ctx, 12*time.Second)
	go func() {
		defer cancel()
		result, err := a.JobAction(ctx, c.who, c.s.sid, request)
		text := ""
		if err != nil {
			text = err.Error()
		} else {
			text = renderJobResponse(result)
		}
		a.mu.Lock()
		a.answer(c, text)
		a.mu.Unlock()
	}()
}
