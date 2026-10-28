package autonomy

import "testing"

func TestSchema2RemovesAutomaticChatFactsAndPreservesExplicitBeliefs(t *testing.T) {
	s := &State{SchemaVersion: 1, Beliefs: []Belief{
		{Subject: "最近和主人聊到", Value: "整理上下文", Source: "chat:main"},
		{Subject: "和对方的约定", Value: "回访", Source: "followup:main"},
		{Subject: "和对方的约定", Value: "提醒", Source: "agenda:a1"},
		{Subject: "主人的偏好", Value: "咖啡", Source: "Mind"},
	}, Events: []Event{{Kind: EventConversation}, {Kind: EventCommitment, Source: "followup:main"}, {Kind: EventExternal, Source: "machine:n"}}}
	s.Normalize()
	if s.SchemaVersion != 2 || len(s.Beliefs) != 1 || s.Beliefs[0].Subject != "主人的偏好" || !s.Events[0].Processed || !s.Events[1].Processed || s.Events[2].Processed {
		t.Fatalf("bad migration: %+v", s)
	}
	rev := s.Revision
	s.Normalize()
	if s.Revision != rev {
		t.Fatal("migration repeated")
	}
}

func TestNormalizeDoesNotDowngradeFutureSchema(t *testing.T) {
	s := &State{SchemaVersion: SchemaVersion + 1, Agenda: []AgendaItem{{Title: "future"}}}
	s.Normalize()
	if s.SchemaVersion != SchemaVersion+1 || s.Agenda[0].Kind != "" || s.Revision != 0 {
		t.Fatal("future state modified")
	}
}
