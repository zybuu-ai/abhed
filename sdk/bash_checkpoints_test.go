package abhed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// An SDK agent hands bash checkpoints to no one, so tools.bash_checkpoints
// "required" refuses its commands rather than running them unrecorded.
func TestSDKRefusesCommandsUnderRequiredCheckpoints(t *testing.T) {
	t.Setenv("ABHED_TRUST_WORKSPACE", "1") // the test wrote this configuration
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame := `{"choices":[{"delta":{"content":"done"}}]}`
		if calls.Add(1) == 1 {
			args, _ := json.Marshal(map[string]string{"command": "touch ran.txt", "description": "run"})
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
			frame = string(call)
		}
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", frame)
	}))
	defer srv.Close()
	cfg := `{"tools":{"bash_checkpoints":"required"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen strings.Builder
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: ws, ConfigDir: ws, Mode: "bypass",
		OnEvent: func(ev abhed.Event) { mu.Lock(); seen.Write(ev.Payload); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Run(context.Background(), "run it"); err != nil {
		t.Fatal(err)
	}
	_ = a.Flush(context.Background())
	if _, err := os.Stat(filepath.Join(ws, "ran.txt")); err == nil {
		t.Fatal("the command ran")
	}
	mu.Lock()
	got := seen.String()
	mu.Unlock()
	if !strings.Contains(got, "tools.bash_checkpoints") {
		t.Fatalf("no refusal recorded:\n%s", got)
	}
}
