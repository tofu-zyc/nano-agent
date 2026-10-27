package proto

import (
	"encoding/json"
	"testing"
)

// C 内核真实输出的帧必须能被 proto.Msg 解出来。字段名以 kernel/src/main.c 为准，
// 改那边必须同步这里；kernel/tests/test_host.py 会断言真实 exec 帧里的字段名，
// 这个测试保证 Go 侧接受同样的形状。
func TestCExecWireCompatibleWithProtoMsg(t *testing.T) {
	// exec：顶层用 sandbox_mode（字符串）。顶层 sandbox 是 hello 的能力对象，
	// 写成字符串会让读取失败、断开内核连接。
	frames := []string{
		`{"t":"exec","sid":"s1","id":"c1","name":"Skill","args":{"name":"demo"},"run_id":"r1-abc","turn_id":1,"timeout_ms":300000,"max_output":65536,"sandbox_mode":"off"}`,
		`{"t":"exec","sid":"s1","id":"c2","name":"Read","args":{"path":"a"},"workspace":"box:~/proj","run_id":"r1-abc","turn_id":1,"timeout_ms":30000,"max_output":65536,"sandbox_mode":"auto"}`,
	}
	for _, f := range frames {
		var m Msg
		if err := json.Unmarshal([]byte(f), &m); err != nil {
			t.Fatalf("C 的 exec 帧解不开（%s）：%v", f, err)
		}
		if m.T != TExec || m.ID == "" || m.Name == "" || m.MaxOutput == 0 {
			t.Fatalf("exec 帧字段不对：%+v", m)
		}
		if m.SandboxMode != "off" && m.SandboxMode != "auto" {
			t.Fatalf("sandbox_mode 没读到：%+v", m)
		}
		if m.Sandbox != nil {
			t.Fatalf("exec 帧不该带 sandbox 对象：%+v", m.Sandbox)
		}
	}
	// hello 的 sandbox 仍然是结构化的能力对象
	var h Msg
	if err := json.Unmarshal([]byte(`{"t":"hello","v":11,"node":"n1","sandbox":{"landlock":7,"mode":"auto","effective":"full"}}`), &h); err != nil {
		t.Fatalf("hello 解不开：%v", err)
	}
	if h.Sandbox == nil || h.Sandbox.Landlock != 7 || h.Sandbox.Mode != "auto" || h.Sandbox.Effective != "full" {
		t.Fatalf("hello 的 sandbox 能力对象：%+v", h.Sandbox)
	}
	// input：请求模式用 sandbox_mode
	var in Msg
	if err := json.Unmarshal([]byte(`{"t":"input","sid":"s1","mode":"auto","sandbox_mode":"on","max_parallel_tools":2}`), &in); err != nil {
		t.Fatalf("input 解不开：%v", err)
	}
	if in.SandboxMode != "on" || in.MaxParallelTools != 2 {
		t.Fatalf("input 字段不对：%+v", in)
	}
}
