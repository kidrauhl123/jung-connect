package discord

import "testing"

func TestProgressReplyContext_NoReference(t *testing.T) {
	p := &Platform{}
	rc := replyContext{channelID: "c", messageID: "m"}
	if rc.referenceForChunk("first", 0) == nil || rc.referenceForChunk("all", 1) == nil {
		t.Fatal("final ctx should carry reference")
	}
	prc, ok := p.ProgressReplyContext(rc).(replyContext)
	if !ok {
		t.Fatalf("unexpected type %T", p.ProgressReplyContext(rc))
	}
	for _, mode := range []string{"first", "all", "off"} {
		if prc.referenceForChunk(mode, 0) != nil {
			t.Fatalf("progress ctx mode=%s carries reference", mode)
		}
	}
	if rc.progress {
		t.Fatal("original ctx mutated")
	}
	if got := p.ProgressReplyContext("other"); got != "other" {
		t.Fatalf("non-replyContext changed: %v", got)
	}
}
