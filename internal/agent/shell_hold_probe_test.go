package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"

	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A model with no secret(...) rule must not learn a stored value from what a
// running shell's read holds back: the shape of every read is the same
// whatever the text says, so a value cannot be guessed a character at a time.
func TestShellReadHoldRevealsNothing(t *testing.T) {
	const standIn = "zq-standin-4f7a9c2e"
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("STAND_IN", standIn); err != nil {
		t.Fatal(err)
	}
	l, _ := suggestLoop(t, &suggestStub{})
	l.Recorder.Redact = vault.Redactor()
	b := &Background{loop: l}

	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789-"
	shape := func(text string, gap bool) [2]int {
		sh := &shellState{}
		r := tools.ShellRead{Text: text}
		if gap {
			r.Dropped = 1
		}
		out, skipped := sh.redactRead(b, r, false)
		return [2]int{len(out), int(skipped)}
	}
	// The attack: extend a known prefix by the one character whose read is held.
	known := ""
	for range len(standIn) {
		var hit []byte
		for i := range len(alphabet) {
			if s := shape("filler line\n"+known+alphabet[i:i+1], false); s[0] <= len("filler line\n") {
				hit = append(hit, alphabet[i])
			}
		}
		if len(hit) != 1 {
			break
		}
		known += string(hit)
	}
	if known != "" {
		t.Errorf("held-back output gave away the value's start: %q", known)
	}
	// A probe at the end of a read, or at its start after a gap, reads the
	// same as other text of its length.
	filler := strings.Repeat("z", 600) + " rest of the line\n"
	for _, gap := range []bool{false, true} {
		want := shape(filler, gap)
		for _, probe := range []string{"zq-st", "e", "2e", "9c2e", "zq-standin-4f7a9c2"} {
			text := filler[:len(filler)-len(probe)] + probe
			if gap {
				text = probe + filler[len(probe):]
			}
			if got := shape(text, gap); got != want {
				t.Errorf("gap %v: probe %q read as %v, other text as %v", gap, probe, got, want)
			}
		}
	}
}

// End to end: a shell given no secret prints a guess at a stored value's
// start and waits; what shell_output shows does not depend on the guess.
func TestShellOutputProbeSeesNoDifference(t *testing.T) {
	probe := func(guess string) string {
		a := &stepAdapter{steps: []func(model.Request) scriptedTurn{
			func(model.Request) scriptedTurn {
				return scriptedTurn{calls: []model.ToolCall{{ID: "c1", Name: "bash", Args: json.RawMessage(
					`{"command":"printf '` + guess + `'; sleep 5","description":"probe","run_in_background":true}`)}}}
			},
			func(req model.Request) scriptedTurn {
				time.Sleep(2 * shellQuietRelease)
				return scriptedTurn{calls: []model.ToolCall{{ID: "c2", Name: "shell_output", Args: json.RawMessage(
					`{"shell_id":"` + lastShellID(req) + `"}`)}}}
			},
			func(req model.Request) scriptedTurn {
				return scriptedTurn{calls: []model.ToolCall{{ID: "c3", Name: "shell_kill", Args: json.RawMessage(
					`{"shell_id":"` + lastShellID(req) + `"}`)}}}
			},
		}}
		r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, a)
		if _, err := r.l.Run(context.Background(), "go"); err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, req := range a.reqs {
			for _, m := range req.Messages {
				if m.Role == model.RoleTool && strings.Contains(m.Content, "running ·") {
					return m.Content
				}
			}
		}
		t.Fatalf("no read of the running shell for %q", guess)
		return ""
	}
	// tok-9f8e starts the rig's stored value; tok-0f8e does not.
	for _, guess := range []string{"tok-0f8e", "tok-9f8e"} {
		if got := probe(guess); !strings.Contains(got, "\n"+guess+"\n") {
			t.Errorf("probe %q: the read held it back, which tells the model it starts a value:\n%s", guess, got)
		}
	}
}
