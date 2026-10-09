package mcp

import (
	"context"
	"strings"
	"testing"
)

// Server a's tool b__x and server a__b's tool x are both mcp__a__b__x, so a
// rule written for one would reach the other: neither is offered.
func TestToolsWhoseNamesAnotherServerClaimsAreLeftOut(t *testing.T) {
	g := NewGateway()
	g.clients["a"] = &Client{name: "a", tools: []ToolDef{{Name: "b__x"}, {Name: "y"}}}
	g.clients["a__b"] = &Client{name: "a__b", tools: []ToolDef{{Name: "x"}}}
	g.clients["ab"] = &Client{name: "ab", tools: []ToolDef{{Name: "z"}}}
	var names []string
	for _, tl := range g.Tools() {
		names = append(names, tl.Name())
	}
	if strings.Join(names, ",") != "mcp__a__y,mcp__ab__z" {
		t.Fatalf("offered %v", names)
	}
	o := g.Overlaps()
	if len(o) != 1 || !strings.Contains(o[0], `server "a"`) || !strings.Contains(o[0], `server "a__b"`) {
		t.Fatalf("overlaps %q", o)
	}
}

// A server whose name is another's and "__" is not started.
func TestOverlappingServerIsNotStarted(t *testing.T) {
	g := NewGateway()
	errs := g.Connect(context.Background(), []ServerConfig{
		{Name: "a", Command: "/nonexistent/abhed-test", Enabled: true},
		{Name: "a__b", Command: "/nonexistent/abhed-test", Enabled: true},
		{Name: "a_b", Command: "/nonexistent/abhed-test", Enabled: true},
	})
	var overlap int
	for _, err := range errs {
		if strings.Contains(err.Error(), "is not started: its name begins with that of server") {
			overlap++
			if !strings.Contains(err.Error(), `"a__b"`) || !strings.Contains(err.Error(), `"a"`) {
				t.Errorf("error %v", err)
			}
		}
	}
	if overlap != 1 {
		t.Fatalf("errors %v", errs)
	}
	if OverlappedBy("a", []string{"a__b"}) != "" || OverlappedBy("a__b", []string{"a"}) != "a" {
		t.Error("OverlappedBy")
	}
}
