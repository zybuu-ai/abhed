package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// A new ACP session records how it started, first: the surface and its mode.
// Loading it again records no second start.
func TestACPNewSessionRecordsItsStart(t *testing.T) {
	r := newStudioRig(t, "", say("ok"))
	id := r.open()
	if stop, _ := r.prompt(id, "hi"); stop != "end_turn" {
		t.Fatalf("stop %q", stop)
	}
	evs := r.events(id)
	starts, _ := r.recorded(id, agent.EvSessionStarted)
	if len(evs) == 0 || evs[0].Type != agent.EvSessionStarted || len(starts) != 1 ||
		starts[0]["surface"] != "acp" || starts[0]["mode"] != "default" || starts[0]["headless"] != false {
		t.Fatalf("starts %v (first event %v)", starts, evs[0].Type)
	}
	var loaded map[string]any
	r.cl.ok("session/close", map[string]any{"sessionId": id}, nil)
	r.cl.ok("session/load", map[string]any{"sessionId": id, "cwd": r.ws, "mcpServers": []any{}}, &loaded)
	if again, _ := r.recorded(id, agent.EvSessionStarted); len(again) != 1 {
		t.Fatalf("a load recorded another start: %v", again)
	}
}

// A new rpc session records how it started, first, with the mode it asked for.
func TestRPCNewSessionRecordsItsStart(t *testing.T) {
	ws := t.TempDir()
	srv := scriptedModel(t, "true")
	cfg := `{"model": {"default": "fake", "providers": {"fake": {"type": "openai-compatible", "base_url": "` + srv.URL + `", "model": "m"}}}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.TrustEnv, "1")
	out := runRPC(t, ws, `{"id":"1","method":"start","mode":"plan"}`, `{"id":"2","method":"prompt","prompt":"hi"}`, `{"id":"3","method":"quit"}`)
	var first *agent.Event
	for _, line := range strings.Split(out, "\n") {
		var r rpcResponse
		if json.Unmarshal([]byte(line), &r) == nil && r.Type == "event" && r.Event != nil {
			first = r.Event
			break
		}
	}
	if first == nil || first.Type != agent.EvSessionStarted {
		t.Fatalf("the first event is not session.started:\n%s", out)
	}
	var p map[string]any
	_ = json.Unmarshal(first.Payload, &p)
	if p["surface"] != "rpc" || p["mode"] != "plan" || p["headless"] != true {
		t.Fatalf("start: %v", p)
	}
}
