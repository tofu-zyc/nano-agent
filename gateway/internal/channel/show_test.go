package channel

import (
	"strings"
	"testing"
)

type showSpy struct {
	events []string
	status string
}

func (s *showSpy) Reasoning(string)        { s.events = append(s.events, "reasoning") }
func (s *showSpy) Content(string)          { s.events = append(s.events, "content") }
func (s *showSpy) EndMessage()             { s.events = append(s.events, "end") }
func (s *showSpy) Tool(string, string)     { s.events = append(s.events, "tool") }
func (s *showSpy) ToolResult(bool, string) { s.events = append(s.events, "result") }
func (s *showSpy) Note(string)             { s.events = append(s.events, "note") }
func (s *showSpy) Status(v string)         { s.status = v }
func (s *showSpy) Close()                  { s.events = append(s.events, "close") }
func TestShowLevels(t *testing.T) {
	for _, tc := range []struct{ level, want string }{{"详细", "reasoning,tool,result,content,end,note,close"}, {"过程", "tool,result,content,end,note,close"}, {"简洁", "content,end,close"}} {
		t.Run(tc.level, func(t *testing.T) {
			s := &showSpy{}
			v := FilterView(s, tc.level)
			v.Reasoning("思考")
			v.Tool("Shell", "secret args")
			if tc.level == "简洁" && !strings.Contains(s.status, "Shell") {
				t.Fatal("no status")
			}
			v.ToolResult(true, "secret output")
			v.Content("答案")
			v.EndMessage()
			v.Note("用量")
			v.Close()
			if strings.Join(s.events, ",") != tc.want || s.status != "" {
				t.Fatal(s)
			}
		})
	}
}
