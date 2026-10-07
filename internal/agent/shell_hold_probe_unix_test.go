//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

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

	// After a gap, the guess ends a read shorter than the hold, which waits;
	// the next read decides the skip. Both read the same for either guess.
	timing := regexp.MustCompile(`running · [^\n]*`)
	gapProbe := func(guess string) string {
		r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{ShellOutputCap: 200}, nil)
		id := r.start(t, "head -c 300 /dev/zero | tr '\\0' .; printf '"+guess+"'; sleep 3; "+
			"head -c 150 /dev/zero | tr '\\0' '#'; printf '\\n'; sleep 30")
		sh, _ := r.l.Background.shell(id)
		waitFor(t, "the first output", func() bool { total, _ := sh.shell.proc.Size(); return total >= 308 })
		first := r.run(t, "shell_output", map[string]any{"shell_id": id}).Content
		waitFor(t, "the second output", func() bool { total, _ := sh.shell.proc.Size(); return total >= 459 })
		time.Sleep(shellQuietRelease + 200*time.Millisecond)
		second := r.run(t, "shell_output", map[string]any{"shell_id": id}).Content
		r.run(t, "shell_kill", map[string]any{"shell_id": id})
		return shellIDIn.ReplaceAllString(timing.ReplaceAllString(first+"\n----\n"+second, "running"), "sh_")
	}
	right, wrong := gapProbe("tok-9f8e"), gapProbe("tok-0f8e")
	if right != wrong || !strings.Contains(right, "dropped") || !strings.Contains(right, "#\n") || strings.Contains(right, "8e") {
		t.Errorf("after a gap, a correct guess reads:\n%s\nand a wrong one:\n%s", right, wrong)
	}

	// The guess starts inside what a cut tail's last line drops.
	lineProbe := func(guess string) string {
		r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, nil)
		id := r.start(t, "head -c 5000 /dev/zero | tr '\\0' .; printf -- '--"+guess+"'; head -c 4086 /dev/zero | tr '\\0' .")
		waitFor(t, "the shell to end", func() bool { ti, _ := r.l.Background.Task(id); return ti.Status == ShellExited })
		ti, _ := r.l.Background.Task(id)
		return ti.LastLine
	}
	if right, wrong := lineProbe("tok-9f8e"), lineProbe("tok-0f8e"); right != wrong || right == "" {
		t.Errorf("a cut tail's last line reads %q for a correct guess, %q for a wrong one", right, wrong)
	}
}

// End to end: a read that ends inside a character is not a gap; the next
// read shows the character whole and everything after it.
func TestShellOutputSplitCharacterIsNoGap(t *testing.T) {
	read := func(id string, after time.Duration) scriptedTurn {
		time.Sleep(after)
		return scriptedTurn{calls: []model.ToolCall{{ID: "r" + after.String(), Name: "shell_output", Args: json.RawMessage(
			`{"shell_id":"` + id + `"}`)}}}
	}
	a := &stepAdapter{steps: []func(model.Request) scriptedTurn{
		func(model.Request) scriptedTurn {
			return scriptedTurn{calls: []model.ToolCall{{ID: "c1", Name: "bash", Args: json.RawMessage(
				`{"command":"printf 'ok \\346\\227'; sleep 3; printf '\\245 more output\\n'; sleep 5","description":"split","run_in_background":true}`)}}}
		},
		func(req model.Request) scriptedTurn { return read(lastShellID(req), 3*shellQuietRelease/2) },
		func(req model.Request) scriptedTurn { return read(lastShellID(req), 4*shellQuietRelease) },
		func(req model.Request) scriptedTurn {
			return scriptedTurn{calls: []model.ToolCall{{ID: "c4", Name: "shell_kill", Args: json.RawMessage(
				`{"shell_id":"` + lastShellID(req) + `"}`)}}}
		},
	}}
	r := newShellRig(t, WakeNotify, policy.ModeBypass, BackgroundPolicy{}, a)
	if _, err := r.l.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var reads []string
	for _, m := range a.reqs[len(a.reqs)-1].Messages {
		if m.Role == model.RoleTool && strings.Contains(m.Content, "running ·") {
			reads = append(reads, m.Content)
		}
	}
	if len(reads) != 2 {
		t.Fatalf("want two reads of the running shell, got %q", reads)
	}
	all := strings.Join(reads, "\n")
	if strings.Contains(all, "not shown") || strings.Contains(all, "�") ||
		!strings.Contains(reads[0], "ok ") || !strings.Contains(reads[1], "日 more output") {
		t.Fatalf("a split character read as a gap:\n%s", all)
	}
}

// A skip after a gap is reported as one, not with the read-limit wording.
func TestShellReadNamesAGapSkip(t *testing.T) {
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("STAND_IN", "gs-standin-5c1e8a"); err != nil {
		t.Fatal(err)
	}
	l, _ := suggestLoop(t, &suggestStub{})
	l.Recorder.Redact = vault.Redactor()
	b := &Background{loop: l, policy: BackgroundPolicy{ShellOutputCap: 1000}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "head -c 3000 /dev/zero | tr '\\0' x; exec sleep 30")
	p, err := tools.StartShellProc(cmd, cancel, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); <-p.Done() }()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if total, _ := p.Size(); total >= 3000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the shell wrote too little")
		}
	}
	time.Sleep(3 * shellQuietRelease / 2)
	task := &bgTask{ID: "s1", Kind: KindShell, done: make(chan struct{}), shell: &shellState{proc: p}}
	got := shellReadResult(b, task).Content
	if !strings.Contains(got, "bytes not shown: skipped after a gap") || strings.Contains(got, "one read returns") {
		t.Fatalf("a gap skip worded as:\n%s", got)
	}
}
