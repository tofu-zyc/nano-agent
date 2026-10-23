package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nano-agent/gateway/internal/persona"
	"nano-agent/gateway/internal/proto"
)

func TestPersonaCommand(t *testing.T) {
	e := setup(t)
	e.say("/persona")
	e.ch.wait(t, "这个会话用的人格：默认人格（default.md）（改过的）")
	e.say("/persona 大肥鱼", from("u2", "李四"))
	e.ch.wait(t, "只有管理员能换人格")
	e.say("/persona 没有这个")
	e.ch.wait(t, "没有这个人格")

	e.say("/persona 2")
	e.ch.wait(t, "换上了「大肥鱼」")
	file := filepath.Join(e.dir, "personas", persona.FileFor(e.sid))
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "大肥鱼") {
		t.Fatalf("会话人格文件：%v %q", err, b)
	}
	e.say("/persona")
	e.ch.wait(t, "这个会话专属的人格：大肥鱼")
	e.say("问题")
	e.k.expect(proto.TInput)
	if msgs := e.roundTrip(t); msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "你是「大肥鱼」") {
		t.Fatalf("模型收到的人格：%q", msgs[0].Content)
	}

	e.say("/persona default")
	e.ch.wait(t, "改回默认人格")
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("会话人格文件还在")
	}
}
