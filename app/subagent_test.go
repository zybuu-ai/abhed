package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// TestSubagentHelper runs abhed -p in the workspace a test below names.
func TestSubagentHelper(t *testing.T) {
	ws := os.Getenv("ABHED_SUBAGENT_WS")
	if ws == "" {
		t.Skip("run by TestHeadlessSubagentAsksAreRefused")
	}
	os.Exit(Main([]string{"-C", ws, "-p", "go", "-output-format", "json"}))
}

// stepModel answers each request with the next step: a tool call as
// name and arguments, or plain text once the script runs out.
func stepModel(t *testing.T, steps [][2]string) string {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if i >= len(steps) {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+strconv.Itoa(i)+
			`","type":"function","function":{"name":"`+steps[i][0]+`","arguments":`+strconv.Quote(steps[i][1])+`}}]}}]}`)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// In -p default mode nobody can be asked: a subagent's rm -rf and its
// ask-rule command are refused as headless, and the parent's record says so.
func TestHeadlessSubagentAsksAreRefused(t *testing.T) {
	url := stepModel(t, [][2]string{
		{"task", `{"prompt":"clean up the workspace","description":"clean up"}`},
		{"bash", `{"command":"rm -rf keep"}`},
		{"bash", `{"command":"touch made.txt"}`},
	})
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	cfg := `{"permissions":{"ask":["bash(touch *)"]},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	for _, dir := range []string{".abhed", "keep"} {
		if err := os.MkdirAll(filepath.Join(ws, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestSubagentHelper$")
	helper.Env = append(os.Environ(), "ABHED_SUBAGENT_WS="+ws, "HOME="+t.TempDir())
	out, _ := helper.CombinedOutput()

	if _, err := os.Stat(filepath.Join(ws, "keep")); err != nil {
		t.Fatalf("a subagent's rm -rf ran in -p: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "made.txt")); err == nil {
		t.Fatalf("a subagent's ask-rule command ran in -p\n%s", out)
	}
	steps := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		var ev agent.Event
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != agent.EvSubagentAction {
			continue
		}
		var a agent.SubagentAction
		_ = json.Unmarshal(ev.Payload, &a)
		if a.Decision == "denied" && a.By == agent.ByHeadless {
			steps[a.Subject] = a.Step
		}
	}
	if steps["rm -rf keep"] != "destructive" || steps["touch made.txt"] != "ask" {
		t.Fatalf("the parent's record does not show the refusals: %v\n%s", steps, out)
	}
}
