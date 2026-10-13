package discord

import (
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestStatusMessagePreferenceOption(t *testing.T) {
	cases := []struct {
		opts         map[string]any
		enabled, set bool
	}{
		{map[string]any{"token": "x"}, false, false},
		{map[string]any{"token": "x", "status_message": true}, true, true},
		{map[string]any{"token": "x", "status_message": false}, false, true},
	}
	for i, c := range cases {
		p, err := New(c.opts)
		if err != nil {
			t.Fatalf("case %d: New: %v", i, err)
		}
		sp, ok := p.(core.StatusMessagePreference)
		if !ok {
			t.Fatalf("platform does not implement StatusMessagePreference")
		}
		en, set := sp.StatusMessagePreference()
		if en != c.enabled || set != c.set {
			t.Fatalf("case %d: got (%v,%v), want (%v,%v)", i, en, set, c.enabled, c.set)
		}
	}
}
