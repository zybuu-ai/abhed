package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

var workingDir = regexp.MustCompile(`Working directory: (.+?)\\n`)

// writeModel answers the first request with a write to rel under the working
// directory named in the system prompt, as an absolute path, then closes;
// sent reports whether the call went out.
func writeModel(t *testing.T, rel string) (srv *httptest.Server, sent *atomic.Bool) {
	t.Helper()
	var n atomic.Int32
	sent = new(atomic.Bool)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		m := workingDir.FindSubmatch(body)
		if n.Add(1) == 1 && m != nil {
			args, _ := json.Marshal(map[string]string{"path": filepath.Join(string(m[1]), rel), "content": "x\n"})
			call, _ := json.Marshal(string(args))
			sent.Store(true)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"write","arguments":`+string(call)+`}}]}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"content":"done"}}]}`)
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, sent
}

// rootsWorkspace is a resolved workspace whose config denies ops/runbooks by a
// relative rule and otherwise lets every call through.
func rootsWorkspace(t *testing.T, url string) string {
	t.Helper()
	managedConfig(t, "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := `{"permissions":{"mode":"bypass","deny":["write(ops/runbooks/**)"]},
		"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

// A relative deny rule holds in a -p run against an absolute path in the
// workspace, which needs the session's roots on the policy.
func TestRunDeniesAbsolutePathByRelativeRule(t *testing.T) {
	srv, sent := writeModel(t, filepath.Join("ops", "runbooks", "x.md"))
	ws := rootsWorkspace(t, srv.URL)
	Main([]string{"-C", ws, "-p", "go"})
	if !sent.Load() {
		t.Fatal("the model never made the write call")
	}
	if _, err := os.Stat(filepath.Join(ws, "ops", "runbooks", "x.md")); err == nil {
		t.Fatal("write(ops/runbooks/**) did not stop an absolute write in a -p run")
	}
}

// The same rule holds in an eval, whose policy is built separately.
func TestEvalDeniesAbsolutePathByRelativeRule(t *testing.T) {
	srv, sent := writeModel(t, filepath.Join("ops", "runbooks", "x.md"))
	ws := rootsWorkspace(t, srv.URL)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	corpus := t.TempDir()
	task := `{"id":"roots","prompt":"go","assertions":[{"type":"file_absent","path":"ops/runbooks/x.md"}]}`
	if err := os.WriteFile(filepath.Join(corpus, "task.json"), []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(t.TempDir(), "report.json")
	evalCmd(ws, corpus, report)
	if !sent.Load() {
		t.Fatal("the model never made the write call")
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"passed": 1`) {
		t.Fatalf("write(ops/runbooks/**) did not stop an absolute write in an eval:\n%s", data)
	}
}
