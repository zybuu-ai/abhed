package app

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
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
	if grant, err := askTrust(strings.NewReader("v\nd\n"), &out, cfg.Workspace); grant || err != nil {
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
