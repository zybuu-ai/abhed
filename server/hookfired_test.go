package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/extension"
)

// A hook that fires for a session the server runs is recorded in that
// session, and one for no session here is recorded nowhere.
func TestServerRecordsHookFiredInItsSession(t *testing.T) {
	store := agent.NewMemStore()
	host := extension.NewHost(nil)
	errs := host.LoadHooks([]extension.HookConfig{{Name: "freeze", Command: "freeze", Events: []extension.Event{extension.EvToolCall}}},
		extension.HookEnv{Run: func(context.Context, string, []byte) (extension.CommandResult, error) {
			return extension.CommandResult{Exit: 2, Stderr: []byte("change freeze")}, nil
		}})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	s := &Server{running: map[string]*liveSession{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		opts: Options{Extensions: host}}
	s.running["s1"] = &liveSession{ID: "s1", Loop: &agent.Loop{Recorder: agent.NewRecorder(store, "s1", "")}}
	s.recordHooks()
	host.OnToolCall(context.Background(), "s1", "bash", json.RawMessage(`{"command":"make"}`))
	host.OnToolCall(context.Background(), "gone", "bash", json.RawMessage(`{"command":"make"}`))
	evs, err := store.Events("s1")
	if err != nil {
		t.Fatal(err)
	}
	var fired []agent.HookFired
	for _, ev := range evs {
		if ev.Type == agent.EvHookFired {
			var h agent.HookFired
			_ = json.Unmarshal(ev.Payload, &h)
			fired = append(fired, h)
		}
	}
	if len(fired) != 1 || fired[0].Verdict != "block" || fired[0].Kind != "command" || fired[0].Reason != "change freeze" ||
		fired[0].ExitCode == nil || *fired[0].ExitCode != 2 {
		t.Fatalf("recorded %+v", fired)
	}
}
