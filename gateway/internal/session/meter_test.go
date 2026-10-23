package session

import (
	"nano-agent/gateway/internal/llm"
	"testing"
)

func TestMeasure(t *testing.T) {
	h := []Msg{{Role: "user", Content: "问题"}, {Role: "tool", Content: "结果"}}
	st := Settings{Meter: Meter{Position: 1, Usage: llm.Usage{PromptTokens: 1000, CachedTokens: 900}, Signature: "a"}}
	c := Measure(st, h, "persona", nil, "a", 128000)
	if c.Total != 1000+Estimate(h[1:]) || c.Persona+c.Tools+c.Summary+c.Dialogue != c.Total || c.ToolResults > c.Dialogue {
		t.Fatalf("%+v", c)
	}
	raw := Measure(Settings{}, h, "persona", nil, "a", 64000)
	if raw.Total != Estimate(h)+TextTokens("persona") || raw.Window != 64000 {
		t.Fatalf("%+v", raw)
	}
	if stale := Measure(st, h, "persona", nil, "b", 64000); stale.Total != raw.Total {
		t.Fatal(stale)
	}
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SaveSettings("test", st); err != nil {
		t.Fatal(err)
	}
	reopened, _ := New(store.Dir())
	if reopened.Settings("test").Meter != st.Meter {
		t.Fatal("meter lost")
	}
}
