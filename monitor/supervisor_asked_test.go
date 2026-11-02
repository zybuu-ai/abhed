package monitor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// Every command the supervisor asked about or denied before stays asked or
// denied: a change that skips text as data must not let any of them through.
func TestSupervisorStaysAsked(t *testing.T) {
	data, err := os.ReadFile("testdata/supervisor_asked.txt")
	if err != nil {
		t.Fatal(err)
	}
	g := driftGuard()
	r := g.Monitor.(Rules)
	r.LookPath = func(string) bool { return true }
	g.Monitor = r
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		said := "rebuild from scratch"
		b, _ := json.Marshal(map[string]any{"command": strings.ReplaceAll(line, "⏎", "\n")})
		c := Case{Remit: said, Said: said, Tool: "bash", Args: string(b), Mutates: true,
			Provisional: policy.Result{Decision: policy.Allow, Step: "mode"},
			Provenance:  TraceSeen(string(b), said, nil), Roots: []string{"/ws"},
			Seen: func(string) bool { return false }}
		if out := g.Review(context.Background(), c); out.After == policy.Allow {
			t.Errorf("no longer asked: %q (%s)", line, out.Verdict.Rationale)
		}
	}
	if n < 1600 {
		t.Fatalf("only %d lines read", n)
	}
}
