package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// §7.12: the engine reads the staged diff itself, leaves out key files,
// withholds stored secrets, records where it goes first, and answers with
// the message.
func TestStudioCommitMessage(t *testing.T) {
	model := newFIMServer(t, "Here is the commit message:\n```\nAdd area helper\n\nCompute a rectangle's area.\n```")
	r := completionRig(t, model.URL, "")
	secret := "stored-secret-value-0123456789abcdef"
	store := filepath.Join(os.Getenv("HOME"), ".abhed", "secrets.json")
	if err := os.WriteFile(store, []byte(`{"FAKE_TOKEN":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ABHED_SECRETS_FILE", store)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", r.ws, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	r.write("area.go", "package geo\n\nfunc Area(w, h int) int { return w * h }\n// "+secret+"\n")
	r.write(".env", "TOKEN=plain-key-value-123\n")
	git("add", "area.go", ".env")
	id := r.open()
	var res struct {
		Message  string `json:"message"`
		Excluded int    `json:"excluded"`
		Empty    string `json:"empty"`
	}
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Message != "Add area helper\n\nCompute a rectangle's area." || res.Excluded != 1 {
		t.Fatalf("%+v", res)
	}
	model.mu.Lock()
	sent := fmt.Sprint(model.bodies)
	model.mu.Unlock()
	if !strings.Contains(sent, "func Area") || strings.Contains(sent, "plain-key-value") || strings.Contains(sent, secret) {
		t.Fatalf("sent %s", sent)
	}
	req, _ := r.recorded(id, agent.EvCommitRequested)
	gen, _ := r.recorded(id, agent.EvCommitMessage)
	if len(req) != 1 || req[0]["files"] != float64(1) || req[0]["excluded"] != float64(1) || len(gen) != 1 || gen[0]["outcome"] != "served" {
		t.Fatalf("requested %v generated %v", req, gen)
	}

	// Nothing staged: nothing sent.
	git("reset", "-q")
	r.cl.ok("_abhed/commitMessage", map[string]any{"sessionId": id}, &res)
	if res.Empty == "" || res.Message != "" {
		t.Fatalf("%+v", res)
	}
}
