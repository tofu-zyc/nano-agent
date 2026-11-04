package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 工具快照的判据只在真的少了工具时才变：新内核什么都不少，判据和 5 补之前一模一样
// （"管理员|内核|在线|MCP|技能"），升级后会话的工具快照不刷新，前缀缓存不受影响。
func TestPermSignatureUnchangedWhenNothingMissing(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	rt, admin, _ := e.a.permOf(e.sid)
	skill := ""
	if spec, ok := e.a.skillSpec(e.sid); ok {
		catalog, _ := json.Marshal(spec)
		skill = fmt.Sprintf("%x", sha256.Sum256(catalog))[:16]
	}
	old := fmt.Sprintf("%t|%s|%t|%s|%s", admin, rt.kernel, e.a.nodes[rt.kernel] != nil, "", skill)
	if got := e.a.permSignature(e.sid); got != old {
		t.Fatalf("新内核什么都不少：判据不变\n现在 %q\n以前 %q", got, old)
	}
	// 旧内核（hello 里没有名单）真的少了 Report、Ticket、External：往后面加，名单变了快照才换
	e.a.nodes[rt.kernel].info.GatewayTools = nil
	got := e.a.permSignature(e.sid)
	if got != old+"|missing:External,Report,Ticket" {
		t.Fatalf("少了工具才加：%q", got)
	}
	e.a.nodes[rt.kernel].info.GatewayTools = []string{"Skill"}
	if got2 := e.a.permSignature(e.sid); got2 == got || !strings.HasPrefix(got2, old+"|missing:") {
		t.Fatalf("交回的名单变了，判据跟着变：%q", got2)
	}
}
