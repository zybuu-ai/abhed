package clitest

import (
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Tests written against the API compile now and skip, rather than fail,
// until the harness is built.
func TestStartSkipsUntilBuilt(t *testing.T) {
	var skipped bool
	t.Run("uses the harness", func(t *testing.T) {
		defer func() { skipped = t.Skipped() }()
		Start(t, Opts{Script: `text "Hello"`})
		t.Error("Start returned without skipping")
	})
	if !skipped {
		t.Fatal("the test was not skipped")
	}
	r := Record{Events: []agent.Event{{Type: agent.EvSessionStarted}, {Type: agent.EvModeChanged}}}
	if !slices.Equal(r.Types(), []agent.EventType{agent.EvSessionStarted, agent.EvModeChanged}) {
		t.Fatalf("types %v", r.Types())
	}
}
