package tools

import (
	"strings"
	"testing"
)

func TestBashRunsAndReportsExit(t *testing.T) {
	s, _ := setup(t)
	res := run(t, Bash{}, s, bashArgs{Command: "echo hello", Description: "print hello"})
	if res.IsError {
		t.Fatal(res.Content)
	}
	if !strings.Contains(res.Content, "hello") {
		t.Fatalf("missing output: %s", res.Content)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %v", res.ExitCode)
	}
}

// A failing command is an observation, not a tool error - the model must be
// able to reason about a failing test run.
func TestBashNonZeroExitIsNotAnError(t *testing.T) {
	s, _ := setup(t)
	res := run(t, Bash{}, s, bashArgs{Command: "exit 3", Description: "fail"})
	if res.IsError {
		t.Fatal("non-zero exit must not be a tool error")
	}
	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("expected exit 3, got %v", res.ExitCode)
	}
	if !strings.Contains(res.Content, "exit 3") {
		t.Fatalf("exit code should be visible: %s", res.Content)
	}
}

// A command ended by a signal reports what a shell would: 128 plus its number,
// as the workbench terminal does.
func TestBashSignalExitIsTheShellsStatus(t *testing.T) {
	s, _ := setup(t)
	res := run(t, Bash{}, s, bashArgs{Command: "kill -9 $$", Description: "die by a signal"})
	if res.ExitCode == nil || *res.ExitCode != 137 {
		t.Fatalf("expected exit 137, got %v: %s", res.ExitCode, res.Content)
	}
}

func TestBashCapturesStderr(t *testing.T) {
	s, _ := setup(t)
	res := run(t, Bash{}, s, bashArgs{Command: "echo oops >&2", Description: "stderr"})
	if !strings.Contains(res.Content, "oops") {
		t.Fatalf("stderr must be captured: %s", res.Content)
	}
}

func TestBashRejectsInteractive(t *testing.T) {
	s, _ := setup(t)
	for _, cmd := range []string{"git rebase -i HEAD~3", "vim file.txt", "less log.txt"} {
		res := run(t, Bash{}, s, bashArgs{Command: cmd, Description: "x"})
		if !res.IsError {
			t.Errorf("%q should be rejected as interactive", cmd)
		}
		if !strings.Contains(res.Content, "hang") {
			t.Errorf("%q error should explain why: %s", cmd, res.Content)
		}
	}
}

func TestBashTimeout(t *testing.T) {
	s, _ := setup(t)
	res := run(t, Bash{}, s, bashArgs{Command: "sleep 5", Description: "sleep", TimeoutMS: 200})
	if !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Fatalf("expected timeout: %s", res.Content)
	}
	if !strings.Contains(res.Content, "timeout_ms") {
		t.Fatalf("timeout error should name the fix: %s", res.Content)
	}
}

func TestBashCwdPersists(t *testing.T) {
	s, dir := setup(t)
	run(t, Bash{}, s, bashArgs{Command: "mkdir -p sub", Description: "mkdir"})
	run(t, Bash{}, s, bashArgs{Command: "cd sub", Description: "cd"})
	if s.Cwd != dir+"/sub" {
		t.Fatalf("cwd should persist, got %s want %s/sub", s.Cwd, dir)
	}
	res := run(t, Bash{}, s, bashArgs{Command: "pwd", Description: "pwd"})
	if !strings.Contains(res.Content, "sub") {
		t.Fatalf("next command should run in the new cwd: %s", res.Content)
	}
}

func TestBashCdOutsideWorkspaceRefused(t *testing.T) {
	s, _ := setup(t)
	before := s.Cwd
	run(t, Bash{}, s, bashArgs{Command: "cd /etc", Description: "escape"})
	if s.Cwd != before {
		t.Fatalf("cd outside workspace should not change cwd: %s", s.Cwd)
	}
}

func TestBashOutputTruncation(t *testing.T) {
	s, _ := setup(t)
	res := run(t, Bash{}, s, bashArgs{
		Command:     "for i in $(seq 1 20000); do echo 'padding line for truncation test'; done",
		Description: "lots of output",
	})
	if !res.Truncated {
		t.Fatal("expected truncation")
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Fatalf("truncation should be visible: %s", res.Content[:200])
	}
}

func TestIsDestructive(t *testing.T) {
	cases := map[string]bool{
		"rm -rf /":                     true,
		"rm -f build/artifact":         true,
		"git push --force origin main": true,
		"git reset --hard HEAD~1":      true,
		"dd if=/dev/zero of=/dev/sda":  true,
		":(){ :|:& };:":                true,
		"chmod -R 777 /":               true,
		"go test ./...":                false,
		"npm install":                  false,
		"git status":                   false,
		"rm file.txt":                  false,
	}
	for cmd, want := range cases {
		what, got := IsDestructive(cmd)
		if got != want {
			t.Errorf("IsDestructive(%q) = %v (%s), want %v", cmd, got, what, want)
		}
	}
}

// A missing description must not fail the call. It labels the approval prompt;
// refusing over a missing label was a real dead end — a model omitted it twice,
// got the same rejection twice, and abandoned the task rather than running the
// command.
func TestBashDerivesMissingDescription(t *testing.T) {
	s, _ := setup(t)
	// Deliberately no Description: that is the case under test.
	res := run(t, Bash{}, s, bashArgs{Command: "echo hello"})

	if res.IsError {
		t.Fatalf("a missing description failed the command: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hello") {
		t.Errorf("command did not run: %s", res.Content)
	}
}

func TestSummarizeCommandIsBounded(t *testing.T) {
	got := summarizeCommand(strings.Repeat("x", 300))
	if len(got) > 100 {
		t.Errorf("label is %d chars, want it bounded", len(got))
	}
	multi := summarizeCommand("first line\nsecond line")
	if strings.Contains(multi, "second") {
		t.Errorf("label spans lines: %q", multi)
	}
}
