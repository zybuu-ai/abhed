package abhed

import (
	"context"
	"testing"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The option reaches the session the agent's tools run in.
func TestSyntaxCheckOptionReachesTheSession(t *testing.T) {
	p := &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}
	for opt, want := range map[string]tools.SyntaxMode{"": tools.SyntaxRefuse, "report": tools.SyntaxReport, "off": tools.SyntaxOff} {
		a, err := New(context.Background(), Options{Workspace: t.TempDir(), Provider: p, SyntaxCheck: opt})
		if err != nil {
			t.Fatal(err)
		}
		if got := a.loop.Session.Syntax; got != want {
			t.Errorf("%q: session mode %v, want %v", opt, got, want)
		}
		a.Close()
	}
}
