package channel

import (
	"context"
	"fmt"
	"strings"
)

// JobCard is a bounded projection of the original execution. Buttons carry
// opaque Gateway tokens, never a mutable chat binding or a raw command.
type JobCard struct {
	JobID      string
	Node       string
	Status     string
	Summary    string
	Background bool
	Finished   bool
	Detail     string
	Buttons    []JobButton
}
type JobButton struct{ Label, Token string }
type JobPresenter interface {
	SendJob(context.Context, string, JobCard) (string, error)
	UpdateJob(context.Context, string, string, JobCard) error
}

const ActionJob = "job"
const JobTokenPrefix = "j:"

func validJobButton(token string) bool {
	if len(token) != 26 || !strings.HasPrefix(token, JobTokenPrefix) {
		return false
	}
	for _, r := range token[2:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func JobCardText(c JobCard) string {
	mode := "前台"
	if c.Background {
		mode = "后台"
	}
	text := fmt.Sprintf("执行任务 · %s · %s\n%s\n%s · %s", c.Status, mode, c.Summary, c.JobID, c.Node)
	if c.Detail != "" {
		text += "\n" + c.Detail
	}
	if !c.Finished && !c.Background {
		text += "\n可以继续等待，或转后台后继续聊天。"
	}
	text += "\n查看：/job " + c.JobID + "\n日志：/job " + c.JobID + " output"
	if !c.Finished {
		if !c.Background {
			text += "\n转后台：/bg " + c.JobID
		}
		text += "\n停止：/job " + c.JobID + " cancel"
	}
	return text
}
