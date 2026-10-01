package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/managed"
)

func workspaceAgent(t *testing.T, ws, name, body string) {
	t.Helper()
	dir := filepath.Join(ws, ".abhed", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The prompt, abhed trust and doctor show each workspace definition's name,
// model and tools, and a grant covers exactly the definitions reviewed.
func TestTrustShowsAgentDefinitions(t *testing.T) {
	_, ws := trustWorkspace(t, `{}`)
	workspaceAgent(t, ws, "reviewer.md", "---\nname: reviewer\ndescription: reviews\nmodel: remote\ntools: [Read, Grep]\n---\nReview.")
	cfg, err := config.LoadWith(ws, config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Workspace.NeedsDecision() {
		t.Fatalf("new definitions asked nothing: %+v", cfg.Workspace)
	}
	var out bytes.Buffer
	if grant, err := askTrust(strings.NewReader("3\n1\n"), &out, cfg.Workspace); grant || err != nil {
		t.Fatalf("grant %v err %v", grant, err)
	}
	for _, want := range []string{"reviewer  model remote  tools Read, Grep", "Trust this file and these definitions?", "--- " + filepath.Join(ws, ".abhed", "agents", "reviewer.md")} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("prompt lacks %q:\n%s", want, out.String())
		}
	}

	var show bytes.Buffer
	if code := trustCmd(ws, nil, &show); code != 0 {
		t.Fatalf("show exited %d", code)
	}
	for _, want := range []string{"agents      1 definition(s)", "trust       NOT TRUSTED", "reviewer  model remote"} {
		if !strings.Contains(show.String(), want) {
			t.Errorf("abhed trust lacks %q:\n%s", want, show.String())
		}
	}
	if code := trustCmd(ws, []string{"grant", "-agents-sha256", strings.Repeat("0", 64)}, io.Discard); code != 1 {
		t.Fatalf("a grant for definitions not on disk exited %d", code)
	}
	if st, _ := config.InspectWorkspace(ws); st.AgentsTrusted {
		t.Fatal("a grant for other definitions trusted them")
	}
	st, _ := config.InspectWorkspace(ws)
	var granted bytes.Buffer
	if code := trustCmd(ws, []string{"grant", "-agents-sha256", st.AgentsSHA256}, &granted); code != 0 || !strings.Contains(granted.String(), "Trusted 1 agent definition") {
		t.Fatalf("grant exited %d:\n%s", code, granted.String())
	}
	if st, _ := config.InspectWorkspace(ws); !st.AgentsTrusted || st.AgentsReason != "stored" {
		t.Fatalf("after the grant: %+v", st)
	}
	doctor, _ := stdoutOf(t, func() int { return newApp().doctor(ws) })
	if !strings.Contains(doctor, "agents      trusted — 1 definition(s)") {
		t.Fatalf("doctor does not report the definitions:\n%s", doctor)
	}
}

// The command line offers the operator's agent definitions, and a trusted
// workspace's, on the task tool; an untrusted workspace's are not offered.
func TestCLIOffersAgentDefinitions(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	home, ws := trustWorkspace(t, `{}`)
	user := `{"sandbox":{"min_tier":"none"},"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` +
		srv.URL + `","model":"m","context_window":8192}}}}`
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceAgent(t, home, "operators.md", "---\ndescription: the operator's role\n---\nDo it.\n")
	workspaceAgent(t, ws, "repos.md", "---\ndescription: the repository's role\n---\nDo it.\n")
	for _, c := range []struct {
		args []string
		repo bool
	}{
		{[]string{"-C", ws, "-p", "hi"}, false},
		{[]string{"-C", ws, "-trust-workspace", "-p", "hi"}, true},
	} {
		mu.Lock()
		bodies = nil
		mu.Unlock()
		cmd := mainHelper(c.args)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %v\n%s", c.args, err, out.String())
		}
		mu.Lock()
		sent := strings.Join(bodies, "\n")
		mu.Unlock()
		if !strings.Contains(sent, "operators — the operator's role") {
			t.Fatalf("%v: the operator's definition is not offered", c.args)
		}
		if strings.Contains(sent, "repos — the repository's role") != c.repo {
			t.Fatalf("%v: the workspace's definition offered = %v", c.args, !c.repo)
		}
	}
}

// doctor names a managed definition that is not read because of its case.
func TestDoctorWarnsManagedCase(t *testing.T) {
	_, ws := trustWorkspace(t, `{}`)
	dir := t.TempDir()
	old := managed.AgentsDir
	managed.AgentsDir = dir
	defer func() { managed.AgentsDir = old }()
	if err := os.WriteFile(filepath.Join(dir, "sec.MD"), []byte("---\ndescription: d\n---\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := stdoutOf(t, func() int { return newApp().doctor(ws) })
	if !strings.Contains(out, "sec.MD is not read") {
		t.Fatalf("doctor does not name the managed file:\n%s", out)
	}
}
