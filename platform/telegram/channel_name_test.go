package telegram

import (
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func TestParseTelegramChannelKey(t *testing.T) {
	tests := []struct {
		in        string
		wantChat  int64
		wantThread int
		wantOK    bool
	}{
		{"-1001234567890", -1001234567890, 0, true},
		{"-1001234567890:55", -1001234567890, 55, true},
		{"12345:1", 12345, 1, true},
		{"", 0, 0, false},
		{"not-a-number", 0, 0, false},
		{"-100:abc", 0, 0, false},
		{"  -1001:42  ", -1001, 42, true},
	}
	for _, tt := range tests {
		chat, thread, ok := parseTelegramChannelKey(tt.in)
		if ok != tt.wantOK || chat != tt.wantChat || thread != tt.wantThread {
			t.Errorf("parseTelegramChannelKey(%q) = (%d,%d,%v), want (%d,%d,%v)",
				tt.in, chat, thread, ok, tt.wantChat, tt.wantThread, tt.wantOK)
		}
	}
}

func TestSanitizeWorkspaceChannelName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"my-project", "my-project"},
		{"  hello / world  ", "hello - world"},
		{"专题/名称", "专题-名称"},
		{"a\\b:c", "a-b-c"},
		{"...", ""},
		{"", ""},
		{"  foo\tbar\n", "foo bar"},
	}
	for _, tt := range tests {
		if got := sanitizeWorkspaceChannelName(tt.in); got != tt.want {
			t.Errorf("sanitizeWorkspaceChannelName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRememberChannelNamesFromMessage_ForumTopicReply(t *testing.T) {
	p := &Platform{channelNameCache: make(map[string]channelNameCacheEntry)}
	msg := &models.Message{
		MessageThreadID: 55,
		Chat: models.Chat{
			ID:      -100999,
			Type:    models.ChatTypeSupergroup,
			Title:   "Dev Forum",
			IsForum: true,
		},
		ReplyToMessage: &models.Message{
			ForumTopicCreated: &models.ForumTopicCreated{Name: "model-profiler"},
		},
	}
	p.rememberChannelNamesFromMessage(msg)

	key := buildChannelKey(-100999, 55)
	name, ok := p.lookupCachedChannelName(key)
	if !ok || name != "model-profiler" {
		t.Fatalf("topic cache = (%q,%v), want model-profiler", name, ok)
	}
	chatName, ok := p.lookupCachedChannelName("-100999")
	if !ok || chatName != "Dev Forum" {
		t.Fatalf("chat cache = (%q,%v), want Dev Forum", chatName, ok)
	}
}

func TestResolveChannelName_UsesTopicCache(t *testing.T) {
	stub := newStubTelegramBot()
	stub.chatInfo = &models.ChatFullInfo{ID: -100999, Title: "Dev Forum", IsForum: true}
	p := &Platform{
		bot:              stub,
		channelNameCache: make(map[string]channelNameCacheEntry),
	}
	p.cacheChannelName("-100999:55", "model-profiler")

	got, err := p.ResolveChannelName("-100999:55")
	if err != nil {
		t.Fatalf("ResolveChannelName: %v", err)
	}
	if got != "model-profiler" {
		t.Fatalf("got %q, want model-profiler", got)
	}
	if stub.getChatCalls != 0 {
		t.Fatalf("expected cache hit without GetChat, got %d calls", stub.getChatCalls)
	}
}

func TestResolveChannelName_PlainChatGetChat(t *testing.T) {
	stub := newStubTelegramBot()
	stub.chatInfo = &models.ChatFullInfo{ID: -1001, Title: "Team Chat"}
	p := &Platform{
		bot:              stub,
		channelNameCache: make(map[string]channelNameCacheEntry),
	}
	got, err := p.ResolveChannelName("-1001")
	if err != nil {
		t.Fatalf("ResolveChannelName: %v", err)
	}
	if got != "Team Chat" {
		t.Fatalf("got %q, want Team Chat", got)
	}
	if stub.getChatCalls != 1 {
		t.Fatalf("GetChat calls = %d, want 1", stub.getChatCalls)
	}
	// Second call should hit cache.
	got2, err := p.ResolveChannelName("-1001")
	if err != nil || got2 != "Team Chat" {
		t.Fatalf("cached resolve: got (%q,%v)", got2, err)
	}
	if stub.getChatCalls != 1 {
		t.Fatalf("expected cache to skip GetChat, calls=%d", stub.getChatCalls)
	}
}

func TestResolveChannelName_GeneralTopicFallback(t *testing.T) {
	stub := newStubTelegramBot()
	stub.chatInfo = &models.ChatFullInfo{ID: -1002, Title: "Forum", IsForum: true}
	p := &Platform{
		bot:              stub,
		channelNameCache: make(map[string]channelNameCacheEntry),
	}
	got, err := p.ResolveChannelName("-1002:1")
	if err != nil {
		t.Fatalf("ResolveChannelName: %v", err)
	}
	if got != "General" {
		t.Fatalf("got %q, want General", got)
	}
}

func TestChannelNameCacheExpiry(t *testing.T) {
	p := &Platform{channelNameCache: make(map[string]channelNameCacheEntry)}
	p.channelNameCacheMu.Lock()
	p.channelNameCache["k"] = channelNameCacheEntry{
		name:      "old",
		expiresAt: time.Now().Add(-time.Second),
	}
	p.channelNameCacheMu.Unlock()
	if _, ok := p.lookupCachedChannelName("k"); ok {
		t.Fatal("expected expired cache miss")
	}
}
