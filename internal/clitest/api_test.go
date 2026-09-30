package clitest

import (
	"os"
	"slices"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

func TestRecordTypes(t *testing.T) {
	r := Record{Events: []agent.Event{{Type: agent.EvSessionStarted}, {Type: agent.EvModeChanged}}}
	if !slices.Equal(r.Types(), []agent.EventType{agent.EvSessionStarted, agent.EvModeChanged}) {
		t.Fatalf("types %v", r.Types())
	}
}
