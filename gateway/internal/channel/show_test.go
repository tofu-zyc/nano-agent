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

type showLevelSpy struct {
	showSpy
	level string
}

func (s *showLevelSpy) SetShow(level string) { s.level = level }

// 要按档位排版的窗口在打开时就知道档位（包括详细档）
func TestShowLevelIsAnnounced(t *testing.T) {
	for _, level := range []string{"详细", "过程", "简洁"} {
		s := &showLevelSpy{}
		FilterView(s, level)
		if s.level != level {
			t.Fatalf("%s：收到 %q", level, s.level)
		}
	}
	s := &showLevelSpy{}
	FilterView(s, "")
	if s.level != "过程" {
		t.Fatalf("默认档位：%q", s.level)
	}
}
