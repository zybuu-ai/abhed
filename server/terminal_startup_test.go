package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// plantStartupFiles points the session's policy at home and writes a
// .bashrc and .zshrc there that run a denied command leaving marker.
func plantStartupFiles(t *testing.T, live *liveSession, home, marker string) {
	t.Helper()
	live.Loop.Policy.Home = func() (string, bool) { return home, true }
	for _, n := range []string{".bashrc", ".zshrc"} {
		if err := os.WriteFile(filepath.Join(home, n), []byte("touch '"+marker+"'\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// deniedLine reports whether the record holds command as requested and then
// denied, and no approval for it.
func deniedLine(evs []agent.Event, command string) bool {
	ids := map[string]bool{}
	for _, e := range evs {
		var a agent.ActionRequested
		var args struct {
			Command string `json:"command"`
		}
		if e.Type == agent.EvActionRequested && json.Unmarshal(e.Payload, &a) == nil && json.Unmarshal(a.Args, &args) == nil && args.Command == command {
			ids[a.CallID] = true
		}
	}
	denied := false
	for _, e := range evs {
		var p struct {
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || !ids[p.CallID] {
			continue
		}
		switch e.Type {
		case agent.EvActionApproved:
			return false
		case agent.EvActionDenied:
			denied = true
		}
	}
	return denied
}

func typedShells() []string {
	lines := []string{"bash"}
	if _, err := exec.LookPath("zsh"); err == nil {
		lines = append(lines, "exec zsh")
	}
	return lines
}

// A shell typed at the workbench's interactive terminal would run a planted
// .bashrc or .zshrc: the line is refused, and the record says denied.
func TestTypedShellWithPlantedStartupFilesIsRefusedInShellMode(t *testing.T) {
	for _, line := range typedShells() {
		t.Run(line, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			wb := shellBench(t, nil)
			marker := filepath.Join(wb.workspace, "shutdown-rc-ran")
			plantStartupFiles(t, wb.live(), home, marker)
			start := wb.startShell()
			out, _ := wb.typeLines(start.ID, line+"\r", "exit\r", "exit\r")
			if fileExists(marker) {
				t.Fatalf("%q ran the planted start-up file:\n%s", line, out)
			}
			if !strings.Contains(out, "Denied") {
				t.Fatalf("the refusal was not shown:\n%s", out)
			}
			if !deniedLine(wb.events(), line) {
				t.Fatalf("the record does not say %q was denied", line)
			}
		})
	}
}

// The line-by-line terminal judges each line as typed at a terminal too.
func TestTypedShellWithPlantedStartupFilesIsRefusedInLinesMode(t *testing.T) {
	for _, line := range typedShells() {
		t.Run(line, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			wb := shellBench(t, func(c *config.Config) { c.Sandbox.Terminal = "lines" })
			marker := filepath.Join(wb.workspace, "shutdown-rc-ran")
			plantStartupFiles(t, wb.live(), home, marker)
			got := wb.typeLine(ptyStartRequest{Command: line})
			if got.Denied == "" || fileExists(marker) {
				t.Fatalf("%q was not refused: %+v", line, got)
			}
			if !deniedLine(wb.events(), line) {
				t.Fatalf("the record does not say %q was denied", line)
			}
		})
	}
}

// A session's own sandbox judges a typed shell the same way.
func TestTypedShellWithPlantedStartupFilesIsRefusedInASessionSandbox(t *testing.T) {
	r := newPerSessionRig(t)
	id, live := r.open(t, "alice")
	home := t.TempDir()
	plantStartupFiles(t, live, home, filepath.Join(home, "shutdown-rc-ran"))
	for _, line := range []string{"bash", "exec zsh"} {
		body, _ := json.Marshal(ptyStartRequest{Command: line, Cols: 80, Rows: 24})
		rec := r.call(t, "POST", "/v1/sessions/"+id+"/pty", "alice", string(body))
		var got ptyStartResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Denied == "" {
			t.Fatalf("%q was not refused: %d %s", line, rec.Code, rec.Body)
		}
	}
	var evs []agent.Event
	if err := json.Unmarshal(r.call(t, "GET", "/v1/sessions/"+id+"/replay", "alice", "").Body.Bytes(), &evs); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"bash", "exec zsh"} {
		if !deniedLine(evs, line) {
			t.Fatalf("the record does not say %q was denied", line)
		}
	}
}

func (wb *workbench) live() *liveSession {
	wb.t.Helper()
	wb.s.mu.RLock()
	defer wb.s.mu.RUnlock()
	live := wb.s.running[wb.session]
	if live == nil {
		wb.t.Fatal("the session is not running")
	}
	return live
}
