//go:build unix

package app

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/config"
)

// ptyRun starts the CLI on a terminal and collects what it prints.
type ptyRun struct {
	t    *testing.T
	tty  *os.File
	mu   sync.Mutex
	out  strings.Builder
	done chan struct{}
}

func startOnPty(t *testing.T, args []string) *ptyRun {
	t.Helper()
	cmd := mainHelper(args)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	r := &ptyRun{t: t, tty: tty, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		br := bufio.NewReader(tty)
		buf := make([]byte, 4096)
		for {
			n, err := br.Read(buf)
			r.mu.Lock()
			r.out.Write(buf[:n])
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // the helper this test started
		_ = cmd.Wait()
		_ = tty.Close()
	})
	return r
}

func (r *ptyRun) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.String()
}

// waitFor waits until the output holds s, counted from the nth occurrence.
func (r *ptyRun) waitFor(s string, n int) {
	r.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); strings.Count(r.text(), s) < n; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.t.Fatalf("never saw %q:\n%s", s, r.text())
		}
	}
}

func (r *ptyRun) send(s string) {
	_, _ = io.WriteString(r.tty, s)
}

// On a terminal the CLI asks once: it lists what the file would set, shows
// it on request, and a trust is recorded and takes effect at once.
func TestCLIPromptTrustsTheWorkspace(t *testing.T) {
	_, ws := trustWorkspace(t, widening)
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Trust this file?", 1)
	for _, w := range []string{"You have not trusted it yet", "permissions.mode", `"bypass"`, "model.default", "Applied already"} {
		if !strings.Contains(r.text(), w) {
			t.Errorf("the prompt lacks %q:\n%s", w, r.text())
		}
	}
	r.send("v\n")
	r.waitFor("Trust this file?", 2)
	if !strings.Contains(r.text(), `"nope"`) {
		t.Fatalf("view did not show the file:\n%s", r.text())
	}
	r.send("t\n")
	// Trusted, the file's model applies, and it does not exist.
	r.waitFor(`model "nope" is not defined`, 1)
	st, _ := config.InspectWorkspace(ws)
	if !st.Trusted || st.Reason != "stored" {
		t.Fatalf("the trust was not recorded: %+v", st)
	}
}

// Declining is remembered for this content, and the session starts with only
// the tightening settings; the next run does not ask again.
func TestCLIPromptDeclineIsRemembered(t *testing.T) {
	_, ws := trustWorkspace(t, `{"permissions":{"mode":"bypass","deny":["bash(curl*)"]}}`)
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Trust this file?", 1)
	r.send("d\n")
	r.waitFor("Type a task", 1)
	if !strings.Contains(r.text(), "was not trusted when you were asked; ignored permissions.mode") {
		t.Fatalf("no warning after declining:\n%s", r.text())
	}
	st, _ := config.InspectWorkspace(ws)
	if st.Trusted || st.Reason != "declined" {
		t.Fatalf("the decline was not recorded: %+v", st)
	}
	again := startOnPty(t, []string{"-C", ws})
	again.waitFor("Type a task", 1)
	if strings.Contains(again.text(), "Trust this file?") {
		t.Fatalf("asked again about content already declined:\n%s", again.text())
	}
}

// Declining new agent definitions keeps the file the person already trusted.
func TestCLIPromptDeclineAgentsKeepsTrustedFile(t *testing.T) {
	_, ws := trustWorkspace(t, `{"permissions":{"deny":["bash(curl*)"],"mode":"plan"}}`)
	st, _ := config.InspectWorkspace(ws)
	if err := config.GrantTrust(ws, st.SHA256); err != nil {
		t.Fatal(err)
	}
	workspaceAgent(t, ws, "reviewer.md", "---\ndescription: reviews\nmodel: remote\n---\nReview.")
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Trust these definitions?", 1)
	if !strings.Contains(r.text(), "reviewer  model remote") {
		t.Fatalf("the prompt does not show the definition:\n%s", r.text())
	}
	r.send("d\n")
	r.waitFor("Type a task", 1)
	st, _ = config.InspectWorkspace(ws)
	if !st.Trusted || st.Reason != "stored" || st.AgentsTrusted || st.AgentsReason != "declined" {
		t.Fatalf("declining the definitions: %+v", st)
	}
}

// Declining a changed file keeps definitions that were already trusted.
func TestCLIPromptDeclineFileKeepsTrustedAgents(t *testing.T) {
	_, ws := trustWorkspace(t, `{"permissions":{"deny":["bash(curl*)"]}}`)
	workspaceAgent(t, ws, "reviewer.md", "---\ndescription: reviews\n---\nReview.\n")
	st, _ := config.InspectWorkspace(ws)
	if err := config.GrantReviewed(ws, st.Reviewed()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(`{"permissions":{"mode":"bypass"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startOnPty(t, []string{"-C", ws})
	r.waitFor("Trust this file?", 1)
	r.send("d\n")
	r.waitFor("Type a task", 1)
	st, _ = config.InspectWorkspace(ws)
	if st.Trusted || st.Reason != "declined" || !st.AgentsTrusted || st.AgentsReason != "stored" {
		t.Fatalf("declining the changed file: %+v", st)
	}
}
