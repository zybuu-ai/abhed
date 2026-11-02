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
	"sync/atomic"
	"testing"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// Options.Runbook approves the operator's steps: the step made as listed
// runs with no approver, a change outside it is put to Approve, and a
// pinned hash that differs refuses the agent.
func TestRunbookOption(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame := `{"choices":[{"delta":{"content":"done"}}]}`
		if n := calls.Add(1); n <= 2 {
			args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, []string{"a.txt", "b.txt"}[n-1]), "content": "x"})
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprint("c", n), "type": "function",
					"function": map[string]any{"name": "write", "arguments": string(args)}}}}}}})
			frame = string(call)
		}
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", frame)
	}))
	defer srv.Close()
	rb := &abhed.Runbook{Plan: "write a", Steps: []abhed.RunbookStep{
		{Tool: "write", Target: filepath.Join(ws, "a.txt"), Args: json.RawMessage(`{"content":"x"}`)},
	}}
	var asked []string
	opts := abhed.Options{Workspace: ws, Mode: "accept-edits", Runbook: rb,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192},
		Approve: func(_ context.Context, _ string, _ json.RawMessage, d abhed.Decision) (bool, error) {
			asked = append(asked, d.Reason)
			return false, nil
		}}

	pinned := opts
	pinned.Runbook = &abhed.Runbook{Plan: rb.Plan, Steps: rb.Steps, SHA256: strings.Repeat("b", 64)}
	if a, err := abhed.New(context.Background(), pinned); err == nil {
		a.Close()
		t.Fatal("a runbook with another hash was bound")
	}

	a, err := abhed.New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); err != nil {
		t.Fatalf("the step did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "b.txt")); err == nil {
		t.Fatal("a change outside the runbook ran in accept-edits")
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "not in the approved plan") {
		t.Fatalf("asked %q", asked)
	}
}
