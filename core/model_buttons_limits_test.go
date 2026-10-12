package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type limitedButtonPlatform struct {
	stubPlatformEngine
	contents []string
	buttons  [][][]ButtonOption
}

func (p *limitedButtonPlatform) SendWithButtons(_ context.Context, _ any, content string, b [][]ButtonOption) error {
	p.contents = append(p.contents, content)
	p.buttons = append(p.buttons, b)
	return nil
}

func (p *limitedButtonPlatform) ButtonLayoutLimits() (int, int) { return 5, 5 }

func TestModelVendorButtons_PaginatesForLimitedPlatforms(t *testing.T) {
	var models []ModelOption
	for i := 0; i < 55; i++ {
		models = append(models, ModelOption{Name: fmt.Sprintf("gemini-x-%02d", i)})
	}
	agent := &stubStrictModelAgent{models: models}
	p := &limitedButtonPlatform{stubPlatformEngine: stubPlatformEngine{n: "discord"}}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	msg := &Message{SessionKey: "discord:c:u", ReplyCtx: "ctx"}

	e.cmdModel(p, msg, []string{"vendor", "g"})
	e.cmdModel(p, msg, []string{"vendor", "g", "4"})

	if len(p.buttons) != 2 {
		t.Fatalf("sends = %d", len(p.buttons))
	}
	for n, rows := range p.buttons {
		if len(rows) > 5 {
			t.Fatalf("send %d: %d rows > 5", n, len(rows))
		}
		for _, r := range rows {
			if len(r) > 5 {
				t.Fatalf("send %d: row of %d buttons", n, len(r))
			}
		}
	}
	first := p.buttons[0]
	nav := first[len(first)-1]
	if nav[0].Data != "cmd:/model vendor g 2" {
		t.Fatalf("next button = %+v", nav)
	}
	if !strings.Contains(p.contents[0], "(1/4)") || !strings.Contains(p.contents[1], "(4/4)") {
		t.Fatalf("page headers: %q / %q", p.contents[0], p.contents[1])
	}
	if first[0][0].Data != "cmd:/model switch 1" {
		t.Fatalf("first model button = %+v", first[0][0])
	}
	last := p.buttons[1]
	if last[0][0].Data != "cmd:/model switch 49" {
		t.Fatalf("page 4 first button = %+v", last[0][0])
	}
}
