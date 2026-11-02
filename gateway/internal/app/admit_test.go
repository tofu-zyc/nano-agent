package app

import (
	"testing"
	"time"

	"nano-agent/gateway/internal/config"
	"nano-agent/gateway/internal/models"
	"nano-agent/gateway/internal/scene"
)

// admitRun：启动一轮之前的共同检查，按顺序：会话忙、没模型、没内核、场景额度用完。

func noModels(t *testing.T, e *env) {
	t.Helper()
	ms, err := models.Open(t.TempDir(), &config.Config{LLM: config.LLM{Provider: "openai"}})
	if err != nil {
		t.Fatal(err)
	}
	e.a.SetModels(ms, nil)
}

func noKernels(e *env) {
	for k := range e.a.nodes {
		delete(e.a.nodes, k)
	}
}

func admit(e *env, sid string) (bool, string, time.Duration) {
	e.a.mu.Lock()
	defer e.a.mu.Unlock()
	return e.a.admitRun(sid)
}

func TestAdmitRunPasses(t *testing.T) {
	e := setup(t)
	if ok, why, retry := admit(e, mainSID); !ok || why != "" || retry != 0 {
		t.Fatalf("什么都齐了应该通过：%v %q %v", ok, why, retry)
	}
}

func TestAdmitRunBusy(t *testing.T) {
	for name, busy := range map[string]func(*sess){
		"在跑":  func(s *sess) { s.run = &run{} },
		"在排队": func(s *sess) { s.queue = append(s.queue, queued{}) },
		"在整理": func(s *sess) { s.compacting = true },
	} {
		e := setup(t)
		e.a.mu.Lock()
		s := e.a.sessOf(mainSID)
		busy(s)
		e.a.mu.Unlock()
		ok, why, retry := admit(e, mainSID)
		e.a.mu.Lock()
		s.run, s.queue, s.compacting = nil, nil, false
		e.a.mu.Unlock()
		if ok || why != admitBusy || retry != 2*time.Minute {
			t.Fatalf("%s：%v %q %v", name, ok, why, retry)
		}
	}
	// 别的会话不受影响
	e := setup(t)
	e.a.mu.Lock()
	e.a.sessOf(mainSID).compacting = true
	e.a.mu.Unlock()
	if ok, _, _ := admit(e, "tg:other"); !ok {
		t.Fatal("忙的是另一个会话")
	}
	e.a.mu.Lock()
	e.a.sessOf(mainSID).compacting = false
	e.a.mu.Unlock()
}

func TestAdmitRunNoModel(t *testing.T) {
	e := setup(t)
	noModels(t, e)
	if ok, why, retry := admit(e, mainSID); ok || why != admitNoModel || retry != 30*time.Minute {
		t.Fatalf("没模型：%v %q %v", ok, why, retry)
	}
}

func TestAdmitRunNoKernel(t *testing.T) {
	e := setup(t)
	e.a.mu.Lock()
	noKernels(e)
	e.a.mu.Unlock()
	if ok, why, retry := admit(e, mainSID); ok || why != admitNoKernel || retry != 5*time.Minute {
		t.Fatalf("没内核：%v %q %v", ok, why, retry)
	}
}

func TestAdmitRunSceneOver(t *testing.T) {
	e := setup(t)
	ext := extGroup(t, e)
	setScene(t, e, ext, &scene.Rules{Budget: 100})
	if ok, _, _ := admit(e, ext); !ok {
		t.Fatal("额度还没用完")
	}
	e.a.mu.Lock()
	e.a.sceneUsage(ext, 100)
	e.a.mu.Unlock()
	ok, why, retry := admit(e, ext)
	// 现在是 15:08（UTC）：等到下一个本地自然日开始，还有 8 小时 52 分
	if ok || why != admitOver || retry != 8*time.Hour+52*time.Minute {
		t.Fatalf("额度用完：%v %q %v", ok, why, retry)
	}
	// 不在场景里的会话不看额度
	if ok, _, _ := admit(e, mainSID); !ok {
		t.Fatal("主会话没有场景")
	}
}

// 几项同时不满足时按顺序报第一个：忙 > 没模型 > 没内核 > 额度。
func TestAdmitRunOrder(t *testing.T) {
	e := setup(t)
	ext := extGroup(t, e)
	setScene(t, e, ext, &scene.Rules{Budget: 100})
	e.a.mu.Lock()
	e.a.sceneUsage(ext, 100)
	noKernels(e)
	e.a.mu.Unlock()
	if _, why, _ := admit(e, ext); why != admitNoKernel {
		t.Fatalf("没内核在额度前面：%q", why)
	}
	noModels(t, e)
	if _, why, _ := admit(e, ext); why != admitNoModel {
		t.Fatalf("没模型在没内核前面：%q", why)
	}
	e.a.mu.Lock()
	e.a.sessOf(ext).compacting = true
	e.a.mu.Unlock()
	if _, why, _ := admit(e, ext); why != admitBusy {
		t.Fatalf("忙在最前面：%q", why)
	}
	e.a.mu.Lock()
	e.a.sessOf(ext).compacting = false
	e.a.mu.Unlock()
}
