package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type named string

func (n named) Name() string                                        { return string(n) }
func (named) Description() string                                   { return "" }
func (named) Schema() json.RawMessage                               { return json.RawMessage(`{}`) }
func (named) Mutates() bool                                         { return false }
func (named) Run(context.Context, *Session, json.RawMessage) Result { return Result{} }

// SubsetStrict case-folds, matches web_search for WebSearch, expands the one
// wildcard, keeps the registry's order, and names what it could not find.
func TestSubsetStrict(t *testing.T) {
	r := NewRegistry(named("read"), named("web_search"), named("mcp__gh__issue"), named("mcp__gh__pr"), named("mcp__jira__x"), named("bash"))
	sub, missing := r.SubsetStrict([]string{"Bash", "WebSearch", "mcp__gh__*", "READ", "raed", "mcp__none__*", "re*"})
	if got := sub.Names(); !reflect.DeepEqual(got, []string{"read", "web_search", "mcp__gh__issue", "mcp__gh__pr", "bash"}) {
		t.Fatalf("subset: %v", got)
	}
	if !reflect.DeepEqual(missing, []string{"raed", "mcp__none__*", "re*"}) {
		t.Fatalf("missing: %v", missing)
	}
	// Two tools that fold alike are never guessed between.
	amb := NewRegistry(named("web_search"), named("web-search"))
	if _, missing := amb.SubsetStrict([]string{"WebSearch"}); len(missing) != 1 {
		t.Fatalf("an ambiguous name matched: %v", missing)
	}
	if got := r.Without([]string{"Bash", "mcp__gh__*", "nothing"}).Names(); !reflect.DeepEqual(got, []string{"read", "web_search", "mcp__jira__x"}) {
		t.Fatalf("without: %v", got)
	}
	// A subtraction removes every tool an ambiguous name could mean.
	if got := amb.Without([]string{"WebSearch"}).Names(); len(got) != 0 {
		t.Fatalf("an ambiguous disallowed name removed nothing: %v", got)
	}
}
