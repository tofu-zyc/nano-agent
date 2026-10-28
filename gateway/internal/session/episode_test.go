package session

import "testing"

func TestEpisodeKeepsRawHistoryAndStablePrefix(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	sid := "@default/life"
	if err := s.Append(sid, Msg{Role: "user", Content: "old brief", Kind: "life_episode"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(sid, Msg{Role: "assistant", Content: "old reply"}); err != nil {
		t.Fatal(err)
	}
	st := s.Settings(sid)
	st.Summary = "older summary"
	st.Prefix = &Prefix{System: "stable", Ver: 7}
	st.EnvNote = "old env"
	st.MemNote = "old mem"
	st.Recalled = []string{"old"}
	st.Omitted = map[int]string{0: "old"}
	if err := s.SaveSettings(sid, st); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginEpisode(sid); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(sid, Msg{Role: "user", Content: "new brief", Kind: "life_episode"}); err != nil {
		t.Fatal(err)
	}
	s, _ = New(dir)
	h := s.History(sid)
	st = s.Settings(sid)
	if len(h) != 1 || h[0].Content != "new brief" || len(s.Archive(sid)) != 3 || st.Base != 2 || st.Summary != "" || st.EnvNote != "" || st.MemNote != "" || len(st.Recalled) != 0 || len(st.Omitted) != 0 || st.Prefix == nil || st.Prefix.System != "stable" {
		t.Fatalf("episode: %+v %+v", h, st)
	}
}
