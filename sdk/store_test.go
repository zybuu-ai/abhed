package abhed_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
	abhed "github.com/zybuu-ai/abhed/sdk"
	"github.com/zybuu-ai/abhed/store/local"
)

// An embedded agent given the local record writes a chained session there:
// listed under its workspace, verified, held while the agent is open and
// let go by Close, so the desktop app and the command line share one record.
func TestOptionsStoreKeepsAVerifiedRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABHED_SECRETS_FILE", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(home, ".abhed", "records")
	rec, err := abhed.OpenLocalRecord("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: ws, Store: rec,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: srv.URL, Model: "m", ContextWindow: 8192}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "hello there"); err != nil {
		t.Fatal(err)
	}
	other, err := local.Open(local.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Acquire(a.ID()); !errors.Is(err, local.ErrHeldElsewhere) {
		t.Fatalf("an open agent's session was taken: %v", err)
	}
	a.Close()
	l, _ := other.Index().List(local.Filter{Cwd: ws})
	if len(l) != 1 || l[0].ID != a.ID() || l[0].Title != "hello there" {
		t.Fatalf("list: %+v", l)
	}
	if rep, err := other.Verify(a.ID()); err != nil || !rep.OK || rep.Events < 3 {
		t.Fatalf("verify: %+v %v", rep, err)
	}
	if err := other.Acquire(a.ID()); err != nil {
		t.Fatalf("Close did not let the session go: %v", err)
	}
}

// scriptedURL answers each request with the next of frames, then "done".
func scriptedURL(t *testing.T, frames ...string) string {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		frame := `{"choices":[{"delta":{"content":"done"}}]}`
		if n < len(frames) {
			frame = frames[n]
		}
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", frame)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func call(id, name string, args map[string]string) string {
	a, _ := json.Marshal(args)
	c, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": string(a)}}}}}}})
	return string(c)
}

// The review's probe: an embedder's record inside the workspace is refused
// at New, rather than left where the agent's tools reach it.
func TestSDKRecordInTheWorkspaceIsRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	rec, err := abhed.OpenLocalRecord(filepath.Join(ws, "records"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	_, err = abhed.New(context.Background(), abhed.Options{Workspace: ws, Store: rec,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Model: "m", ContextWindow: 8192}})
	if err == nil || !strings.Contains(err.Error(), "record") {
		t.Fatalf("a record in the workspace was accepted: %v", err)
	}
}

// The agent's read, write and bash, sandboxed as the configuration asks,
// cannot reach an SDK agent's record or its blobs. (There is no "!" command
// in the SDK; the CLI's is tested with it.)
func TestSDKAgentCannotReachItsRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	rec, err := abhed.OpenLocalRecord("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	sha, _ := rec.Blobs().Put([]byte("BLOB-CANARY-9171"))
	session := filepath.Join(rec.Dir(), "default", "index.jsonl")
	blob := filepath.Join(rec.Dir(), "default", "blobs", "sha256", sha[:2], sha)
	url := scriptedURL(t,
		call("c1", "read", map[string]string{"path": session}),
		call("c2", "read", map[string]string{"path": blob}),
		call("c3", "write", map[string]string{"path": filepath.Join(rec.Dir(), "default", "planted.jsonl"), "content": "x"}),
		call("c4", "bash", map[string]string{"command": "cat " + blob + " " + session + "; echo x > " + filepath.Join(rec.Dir(), "default", "b.jsonl"), "description": "try"}),
	)
	var mu sync.Mutex
	var seen strings.Builder
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: ws, Store: rec, Sandbox: true, Mode: "bypass",
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: url, Model: "m", ContextWindow: 8192},
		OnEvent:  func(ev abhed.Event) { mu.Lock(); seen.Write(ev.Payload); mu.Unlock() }})
	if err != nil {
		t.Skipf("no process sandbox here: %v", err)
	}
	defer a.Close()
	_, _ = a.Run(context.Background(), "look at the record")
	_ = a.Flush(context.Background())
	mu.Lock()
	got := seen.String()
	mu.Unlock()
	if strings.Contains(got, "BLOB-CANARY-9171") || strings.Contains(got, `"op":"create"`) {
		t.Fatalf("the agent read its record:\n%s", got)
	}
	if strings.Count(got, "Abhed's own state") < 3 {
		t.Fatalf("the file tools did not refuse the record as state:\n%s", got)
	}
	for _, p := range []string{"planted.jsonl", "b.jsonl"} {
		if _, err := os.Stat(filepath.Join(rec.Dir(), "default", p)); err == nil {
			t.Fatalf("the agent wrote %s into its record", p)
		}
	}
}

// A ~/.abhed/records that is a link to elsewhere is used and protected by
// where it really is.
func TestSymlinkedRecordsAreProtectedWhereTheyAre(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABHED_SECRETS_FILE", "")
	real := filepath.Join(home, "elsewhere", "records")
	_ = os.MkdirAll(real, 0o700)
	_ = os.MkdirAll(filepath.Join(home, ".abhed"), 0o700)
	if err := os.Symlink(real, filepath.Join(home, ".abhed", "records")); err != nil {
		t.Fatal(err)
	}
	rec, err := abhed.OpenLocalRecord("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if want, _ := filepath.EvalSymlinks(real); rec.Dir() != want {
		t.Fatalf("the record is at %s, not its real path %s", rec.Dir(), want)
	}
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	// Here the real directory is in a temp folder, where commands can write,
	// so New refuses it outright, naming where it really is.
	_, err = abhed.New(context.Background(), abhed.Options{Workspace: ws, Store: rec,
		Provider: &abhed.Provider{Type: "openai-compatible", BaseURL: "http://127.0.0.1:1", Model: "m", ContextWindow: 8192}})
	if err == nil || !strings.Contains(err.Error(), rec.Dir()) {
		t.Fatalf("a record linked into a temp folder was accepted: %v", err)
	}
	// Wherever it is, the real path is state: the CLI and SDK register it.
	var found bool
	for _, p := range sandboxconfig.StatePaths(config.Config{}, ws) {
		if p == rec.Dir() {
			found = true
			tools.AddStatePath(p)
		}
	}
	if !found {
		t.Fatalf("the state paths leave out the real record %s", rec.Dir())
	}
	p := filepath.Join(rec.Dir(), "default", "index.jsonl")
	if !tools.IsState(p, ws) {
		t.Fatalf("%s is not state", p)
	}
	sess, _ := tools.NewSession(ws)
	if _, err := sess.ReadFile(p); err == nil {
		t.Fatal("the agent's session reads the record through its real path")
	}
}

// A managed record.dir stays state when an embedder hands in another record:
// both are state for the agent.
func TestSDKRecordKeepsTheManagedRecordDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ABHED_SECRETS_FILE", "")
	managed := filepath.Join(home, ".abhed", "org-records")
	cfg := config.Config{Record: config.RecordConfig{Dir: managed}}
	rec, err := abhed.OpenLocalRecord("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	paths := sandboxconfig.StatePaths(cfg, t.TempDir())
	if !slices.Contains(paths, managed) {
		t.Fatalf("no managed dir in %v", paths)
	}
	got, err := abhed.WithRecordStateForTest(cfg, abhed.Options{Workspace: t.TempDir(), Store: rec})
	if err != nil {
		t.Fatal(err)
	}
	paths = sandboxconfig.StatePaths(got, t.TempDir())
	if got.Record.Dir != managed || !slices.Contains(paths, managed) || !slices.Contains(paths, rec.Dir()) {
		t.Fatalf("record.dir %s, state %v", got.Record.Dir, paths)
	}
}
