package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// When the system denies an ask nobody answered in time, StartSpec's
// OnAskTimeout is told which request and tool, and the record says the
// system decided.
func TestOnAskTimeoutIsToldOfTheSystemsDenial(t *testing.T) {
	defer approvalDeadline.Set(approvalDeadline.Set(300 * time.Millisecond))
	work := t.TempDir()
	args, _ := json.Marshal(map[string]any{"path": filepath.Join(work, "m.txt"), "content": "x"})
	st := agent.NewMemStore()
	s := New(Options{Workspace: work, Config: config.Default(), Store: st,
		Adapter:  promptAdapter{calls: map[string]model.ToolCall{"go": {ID: "c1", Name: "write", Args: args}}},
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{})})
	type hit struct{ req, tool string }
	got := make(chan hit, 2)
	ended := make(chan struct{})
	id, err := s.StartSession(context.Background(), StartSpec{Prompt: "go", User: "alice", Tenant: "t1",
		OnAskTimeout: func(req, tool string) { got <- hit{req, tool} },
		OnEnd:        func(string, error) { close(ended) }})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case h := <-got:
		if h.tool != "write" || h.req == "" {
			t.Fatalf("told %+v", h)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnAskTimeout was not called")
	}
	<-ended
	evs, _ := st.Events(id)
	var denied string
	for _, e := range evs {
		if e.Type == agent.EvActionDenied {
			denied = string(e.Payload)
		}
	}
	if !strings.Contains(denied, `"by":"system"`) || !strings.Contains(denied, "no answer within") {
		t.Fatalf("the denial as recorded: %s", denied)
	}
}
