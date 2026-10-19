package abhed

import (
	"context"
	"encoding/json"
	"testing"
)

// The option reaches the loop: the rules by default, with nobody to ask an
// agent without an approver; off when asked; a model judge needs its endpoint.
func TestSupervisorOption(t *testing.T) {
	p := &Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}
	a, err := New(context.Background(), Options{Workspace: t.TempDir(), Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	if g := a.loop.Monitor; g == nil || !g.Unattended {
		t.Fatalf("default supervisor: %+v", g)
	}
	a.Close()

	a, err = New(context.Background(), Options{Workspace: t.TempDir(), Provider: p,
		Approve: func(context.Context, string, json.RawMessage, Decision) (bool, error) { return false, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if g := a.loop.Monitor; g == nil || g.Unattended {
		t.Fatalf("with an approver: %+v", g)
	}
	a.Close()

	a, err = New(context.Background(), Options{Workspace: t.TempDir(), Provider: p, Supervisor: "off"})
	if err != nil {
		t.Fatal(err)
	}
	if a.loop.Monitor != nil {
		t.Fatal("off left a supervisor")
	}
	a.Close()

	if _, err := New(context.Background(), Options{Workspace: t.TempDir(), Provider: p, Supervisor: "model"}); err == nil {
		t.Fatal("a model judge with no endpoint was accepted")
	}
}
