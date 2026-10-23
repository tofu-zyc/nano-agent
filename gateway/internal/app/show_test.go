package app

import "testing"

func TestShowPrecedenceAndPersistence(t *testing.T) {
	e := setup(t)
	e.a.cfg.Channels.Show = "详细"
	e.a.cfg.Telegram.Show = "简洁"
	e.say("/show 过程")
	e.ch.wait(t, "显示档位已设为过程")
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	ep := parseEndpoint("fake:chat")
	for key, r := range e.a.eps {
		if r.Show == "过程" {
			ep = parseEndpoint(key)
		}
	}
	if e.a.showOf(ep) != "过程" {
		t.Fatal("override missing")
	}
	e.a.loadEndpoints()
	if e.a.showOf(ep) != "过程" || e.a.showOf(endpoint{ch: "tg"}) != "简洁" || e.a.showOf(endpoint{ch: "web"}) != "详细" {
		t.Fatal("precedence or persistence")
	}
}
