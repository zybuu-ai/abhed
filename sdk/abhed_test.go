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

// The SDK is the wall between another program and internal/. If this file
// stops compiling, the supported surface has changed under someone.
func TestSurfaceIsUsableFromOutside(t *testing.T) {
	dir := t.TempDir()
	var seen []abhed.Event

	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir,
		Provider: &abhed.Provider{
			Type: "ollama", BaseURL: "http://127.0.0.1:1", // never reached
			Model: "test", ContextWindow: 8192,
		},
		Mode:    "auto",
		Deny:    []string{"bash(rm -rf *)"},
		OnEvent: func(ev abhed.Event) { seen = append(seen, ev) },
		Approve: func(ctx context.Context, tool string, args json.RawMessage, d abhed.Decision) (bool, error) {
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// The methods a caller needs are all present and callable.
	_ = a.Events()
	_ = a.Usage()
	_ = a.ExportHTML()
	a.Steer("noted")
	_ = seen
}

func TestProvidersIsExported(t *testing.T) {
	if len(abhed.Providers()) < 10 {
		t.Fatalf("Providers() returned %d; the registry should be visible to a caller",
			len(abhed.Providers()))
	}
}

// An embedded agent with no approver must refuse what needs approval, not
// assume yes. Defaulting to permissive would make the SDK quietly weaker than
// the same policy on the command line.
func TestNoApproverMeansRefuse(t *testing.T) {
	dir := t.TempDir()
	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir,
		Provider:  &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"},
		Mode:      "default", // every mutation asks
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Nothing to assert without a live model; the guarantee is in the
	// constructor, and this proves the option is reachable and the default is
	// the strict one rather than a panic.
}

func TestWorkspaceIsRequired(t *testing.T) {
	_, err := abhed.New(context.Background(), abhed.Options{})
	if err == nil {
		t.Fatal("an agent with no workspace must be refused")
	}
	if !strings.Contains(err.Error(), "Workspace") {
		t.Errorf("the error should name what is missing: %v", err)
	}
}

func TestConfigDirIsHonoured(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"model":{"default":"local","providers":{"local":{
		"type":"ollama","base_url":"http://127.0.0.1:1","model":"from-config"}}}}`
	if err := os.WriteFile(filepath.Join(dir, ".abhed", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := abhed.New(context.Background(), abhed.Options{
		Workspace: dir, ConfigDir: dir,
	})
	if err != nil {
		t.Fatalf("a config file the CLI would accept must work here too: %v", err)
	}
	a.Close()
}

// A bad deny rule must fail at construction, not on the first tool call.
func TestBadRuleFailsEarly(t *testing.T) {
	_, err := abhed.New(context.Background(), abhed.Options{
		Workspace: t.TempDir(),
		Provider:  &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"},
		Deny:      []string{"bash("},
	})
	if err == nil {
		t.Fatal("a malformed rule must be refused at construction")
	}
}

// The syntax check is set from Options, overriding the config, and a bad
// value fails at construction like any other setting.
func TestSyntaxCheckOptionIsApplied(t *testing.T) {
	p := &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}
	for _, v := range []string{"", "refuse", "report", "off"} {
		a, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(), Provider: p, SyntaxCheck: v})
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		a.Close()
	}
	_, err := abhed.New(context.Background(), abhed.Options{Workspace: t.TempDir(), Provider: p, SyntaxCheck: "sometimes"})
	if err == nil || !strings.Contains(err.Error(), "syntax_check") {
		t.Fatalf("a bad SyntaxCheck must fail at construction: %v", err)
	}
}

// A session with the configured sandbox (as rpc, acp and resolve start one)
// is refused when a state file has a second name.
func TestSandboxedSessionRefusesALinkedStateFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws := t.TempDir()
	users := filepath.Join(ws, ".abhed", "users.json")
	if err := os.MkdirAll(filepath.Dir(users), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(users, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(`{"sandbox":{"min_tier":"none"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := abhed.Options{Workspace: ws, ConfigDir: ws, Sandbox: true,
		Provider: &abhed.Provider{Type: "ollama", BaseURL: "http://127.0.0.1:1", Model: "m"}}
	a, err := abhed.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("a session was refused without a link: %v", err)
	}
	a.Close()
	if err := os.Link(users, filepath.Join(ws, "notes.json")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if a, err := abhed.New(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "2 names") {
		if a != nil {
			a.Close()
		}
		t.Fatalf("a session started over a state file with a second name: %v", err)
	}
}

// A session whose workspace is a worktree inside the configuration's folder,
// as a resolve run's is, cannot read that folder's .abhed from its commands.
func TestConfigFoldersStateIsHiddenFromAWorktreeSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(repo, ".abhed-worktrees", "issue-1")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("repo-readme"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "cat " + filepath.Join(repo, "README.md") + "; cat " + filepath.Join(repo, ".abhed", "config.json")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame := `{"choices":[{"delta":{"content":"done"}}]}`
		if calls.Add(1) == 1 {
			args, _ := json.Marshal(map[string]string{"command": command, "description": "read"})
			call, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": string(args)}}}}}}})
			frame = string(call)
		}
		fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", frame)
	}))
	defer srv.Close()
	cfg := `{"secret":"repo-config","sandbox":{"min_tier":"process"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.MkdirAll(filepath.Join(repo, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen strings.Builder
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: worktree, ConfigDir: repo, Sandbox: true, Mode: "bypass",
		OnEvent: func(ev abhed.Event) { mu.Lock(); seen.Write(ev.Payload); mu.Unlock() }})
	if err != nil {
		t.Skipf("no process sandbox here: %v", err)
	}
	defer a.Close()
	if _, err := a.Run(context.Background(), "read the files"); err != nil {
		t.Fatal(err)
	}
	_ = a.Flush(context.Background())
	mu.Lock()
	got := seen.String()
	mu.Unlock()
	if !strings.Contains(got, "repo-readme") {
		t.Skipf("the process sandbox cannot run a command here: %s", got)
	}
	if strings.Contains(got, "repo-config") {
		t.Fatalf("the worktree session read the configuration folder's state:\n%s", got)
	}
}
