package config

import (
	"fmt"
	"testing"
)

// A workspace may only lower the reasoning bounds, and a managed value binds.
func TestReasoningStallLayers(t *testing.T) {
	limit := func(sec, tok int) string {
		return fmt.Sprintf(`{"limits":{"reasoning_stall_seconds":%d,"reasoning_stall_tokens":%d}}`, sec, tok)
	}
	for _, c := range []struct {
		name, managed, user, ws string
		trusted                 bool
		sec, tok                int
	}{
		{"workspace lowers the user's", "", limit(300, 6000), limit(120, 4000), false, 120, 4000},
		{"workspace cannot raise the user's", "", limit(120, 4000), limit(300, 6000), false, 120, 4000},
		{"workspace cannot raise the default", "", "", limit(900, 9000), false, 0, 0},
		{"workspace lowers the default", "", "", limit(60, 2000), false, 60, 2000},
		{"a workspace zero keeps the user's", "", limit(90, 3000), limit(0, 0), false, 90, 3000},
		{"managed binds over a trusted workspace", limit(120, 4000), "", limit(600, 8000), true, 120, 4000},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.managed != "" {
				withManaged(t, c.managed)
			}
			_, ws := trustHome(t, c.user, c.ws)
			o := LoadOptions{Quiet: true}
			if c.trusted {
				o.Trust = TrustGranted
			}
			cfg, err := LoadWith(ws, o)
			if err != nil {
				t.Fatal(err)
			}
			if l := cfg.Limits; l.ReasoningStallSeconds != c.sec || l.ReasoningStallTokens != c.tok {
				t.Fatalf("seconds %d tokens %d, want %d %d", l.ReasoningStallSeconds, l.ReasoningStallTokens, c.sec, c.tok)
			}
		})
	}
}
