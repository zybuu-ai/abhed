package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type describedTool struct {
	name, desc string
}

func (n describedTool) Name() string            { return n.name }
func (n describedTool) Description() string     { return n.desc }
func (n describedTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (n describedTool) Mutates() bool           { return true }
func (n describedTool) Run(context.Context, *Session, json.RawMessage) Result {
	return Result{Content: n.desc}
}

// A second tool of a name already registered is refused with a warning, and
// the first keeps the name; Replace is the deliberate swap.
func TestAddRefusesADuplicateName(t *testing.T) {
	var warned bytes.Buffer
	old := WarnOut
	WarnOut = &warned
	t.Cleanup(func() { WarnOut = old })

	r := NewRegistry(describedTool{"mcp__gh__issue", "the server's"})
	if r.Add(describedTool{"mcp__gh__issue", "an extension's"}) {
		t.Fatal("a duplicate name was registered")
	}
	if got, _ := r.Get("mcp__gh__issue"); got.Description() != "the server's" {
		t.Fatalf("the first tool lost its name to %q", got.Description())
	}
	if !strings.Contains(warned.String(), `a second tool named "mcp__gh__issue" is not registered`) {
		t.Fatalf("no warning: %q", warned.String())
	}
	if len(r.Names()) != 1 {
		t.Fatalf("names %v", r.Names())
	}
	r.Replace(describedTool{"mcp__gh__issue", "bound"})
	if got, _ := r.Get("mcp__gh__issue"); got.Description() != "bound" || len(r.Names()) != 1 {
		t.Fatalf("Replace: %q %v", got.Description(), r.Names())
	}
}

type loadable struct {
	describedTool
	loaded bool
	used   int64
}

func (l *loadable) Hidden() bool    { return !l.loaded }
func (l *loadable) LastUsed() int64 { return l.used }
func (l *loadable) Unload()         { l.loaded = false }

// Unloading takes the least recently used loaded tools, and nothing else.
func TestUnloadLeastUsedKeepsBuiltins(t *testing.T) {
	a := &loadable{describedTool{"mcp__s__a", ""}, true, 3}
	b := &loadable{describedTool{"mcp__s__b", ""}, true, 1}
	c := &loadable{describedTool{"mcp__s__c", ""}, true, 2}
	hidden := &loadable{describedTool{"mcp__s__d", ""}, false, 0}
	r := NewRegistry(Read{}, a, b, c, hidden)
	if n := r.UnloadLeastUsed(2); n != 2 {
		t.Fatalf("unloaded %d", n)
	}
	if !b.Hidden() || !c.Hidden() || a.Hidden() {
		t.Fatalf("unloaded the wrong ones: a %v b %v c %v", a.Hidden(), b.Hidden(), c.Hidden())
	}
	if n := r.UnloadLeastUsed(10); n != 1 {
		t.Fatalf("unloaded %d more, want only a", n)
	}
	if defs := r.Definitions(); len(defs) != 1 || defs[0].Name != "read" {
		t.Fatalf("offered %v", defs)
	}
}
