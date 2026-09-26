package tools

import (
	"os"
	"os/exec"
	"path/filepath"
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

// A cd is followed with its quoting undone as bash would, so a name that
// Tab completed as web\ app is the folder the shell went to. What cannot be
// read with confidence is not followed, and says why.
func TestDetectCdUndoesQuotingAndSaysWhatItDidNotFollow(t *testing.T) {
	s, dir := setup(t)
	for _, d := range []string{"packages/web app", "it's", `a"b`, "plain"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ cmd, want, why string }{
		{`cd packages/web\ app/`, "packages/web app", ""},
		{`cd "packages/web app"`, "packages/web app", ""},
		{`cd 'packages/web app'`, "packages/web app", ""},
		{`cd packages/'web app'`, "packages/web app", ""},
		{`cd $'packages/web app'`, "packages/web app", ""},
		{`cd it\'s`, "it's", ""},
		{`cd "it's"`, "it's", ""},
		{`cd $'it\'s'`, "it's", ""},
		{`cd a\"b`, `a"b`, ""},
		{`cd "a\"b"`, `a"b`, ""},
		{`cd`, ".", ""},
		{`cd ~`, ".", ""},
		{"  cd\tplain \t", "plain", ""},
		{"cd plain\n", "plain", ""},
		{`cd missing`, "", "no such directory: missing"},
		{`cd file.txt`, "", "no such directory: file.txt"},
		{`cd /etc`, "", "outside the workspace"},
		{`cd .abhed`, "", "Abhed's own state"},
		// Only a line that is just cd and one folder is followed.
		{`ls && cd plain`, "", "only a line that is just `cd <folder>` is"},
		{`mkdir x && cd x`, "", "not followed"},
		{`cd plain && cd packages`, "", "not followed"},
		{`cd plain && cd`, "", "not followed"},
		{`cd plain && cd ""`, "", "not followed"},
		{`ls; cd plain && cd packages`, "", "not followed"},
		{`builtin cd plain && cd packages`, "", "not followed"},
		{`command cd plain`, "", "not followed"},
		{`\cd plain`, "", "not followed"},
		{`{ cd plain; } && cd packages`, "", "not followed"},
		{`(cd plain) && cd packages`, "", "not followed"},
		{`pushd plain >/dev/null`, "", "not followed"},
		{`popd`, "", "not followed"},
		{`CDPATH=plain && cd packages`, "", "not followed"},
		{`# && cd plain`, "", "not followed"},
		{`echo x #&& cd plain`, "", "not followed"},
		{`cd plain; ls`, "", "not followed"},
		{`cd plain | cat`, "", "not followed"},
		{`cd plain &`, "", "not followed"},
		{`cd plain || true`, "", "not followed"},
		{"cd plain\nls", "", "not followed"},
		{`cd -- plain`, "", "not followed"},
		{`cd packages/web app`, "", "not followed"},
		{`cd "$HOME"`, "", "not followed"},
		{"cd `pwd`", "", "not followed"},
		{`cd pla*`, "", "not followed"},
		{`cd "unfinished`, "", "not followed"},
		{`cd $'a\nb'`, "", "not followed"},
		{`cd -`, "", "not followed"},
		{`cd ~/x`, "", "not followed"},
		{`cd ""`, "", "not followed"},
		{`cd $'a\ b'`, "", "not followed"},
		{"cd a\\\nb", "", "not followed"},
		{"cd plain\r", "", "not followed"},
		{"cd plain\v", "", "not followed"},
		{"cd plain\f", "", "not followed"},
		{"cd\nplain", "", "not followed"},
		// A line with no way to change directory says nothing.
		{`ls`, "", ""},
		{`git checkout -b cd-fix`, "", ""},
		{`echo abcd`, "", ""},
		{`ls .`, "", ""},
		{`./run.sh`, "", ""},
		{`echo sourced evaluate`, "", ""},
		// Quoted, escaped or indirect ways to change directory still get the note.
		{`"cd" plain`, "", "not followed"},
		{`'cd' plain`, "", "not followed"},
		{`c\d plain`, "", "not followed"},
		{`builtin "cd" plain`, "", "not followed"},
		{`eval "cd plain"`, "", "not followed"},
		{`eval x`, "", "not followed"},
		{`source env.sh`, "", "not followed"},
		{`. env.sh`, "", "not followed"},
		{`true; . env.sh`, "", "not followed"},
		{"echo `cd plain`", "", "not followed"},
	} {
		s.Cwd = dir
		next, why := detectCd(c.cmd, s)
		got := ""
		if next != "" {
			got = s.Rel(next)
		}
		if got != c.want || (c.why == "") != (why == "") || !strings.Contains(why, c.why) {
			t.Errorf("%s: went to %q, said %q; want %q, saying %q", c.cmd, got, why, c.want, c.why)
		}
	}
}

// shellWord reads a word as bash reads it, or not at all: every word it
// accepts is checked against what bash itself makes of it.
func TestShellWordAgreesWithBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	words := []string{`a\ b`, `"a b"`, `'a b'`, `a'b c'd`, `$'a b'`, `$'it\'s'`, `$'a\\b'`, `$'a\"b'`, `$'a\?b'`,
		`"a\"b"`, `"a\\b"`, `"a\b"`, `"a\zb"`, `a\\b`, `'a\b'`, `a"b"'c'`, `web\ app/`, "\xc3\xa9\\ \xc3\xbc",
		`$'a\ b'`, "a\\\nb", "\"a\\\nb\"", `$'a\nb'`, `$HOME`, `"$HOME"`, "`pwd`", `a*`, `{a,b}`, `a b`, `"open`, `~x`}
	refused := map[string]bool{`$'a\ b'`: true, "a\\\nb": true, "\"a\\\nb\"": true, `$'a\nb'`: true}
	for _, w := range words {
		got, ok := shellWord(w)
		if refused[w] && ok {
			t.Errorf("%q: read as %q, want it refused", w, got)
		}
		if !ok {
			continue
		}
		out, err := exec.Command(bash, "-c", "printf %s "+w).Output()
		if err != nil || string(out) != got {
			t.Errorf("%q: read as %q, bash makes it %q (%v)", w, got, out, err)
		}
	}
}

// The model is told where it still is when its cd was not followed, and a
// cd that did not run, behind a failed command, does not move the tracker.
func TestBashToolReportsACdItDidNotFollow(t *testing.T) {
	s, dir := setup(t)
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := run(t, Bash{}, s, bashArgs{Command: `cd "$PWD"`, Description: "cd"})
	if s.Cwd != dir || !strings.Contains(res.Content, "not followed") || !strings.Contains(res.Content, "still in .") {
		t.Fatalf("unfollowed cd: cwd %s, result %s", s.Cwd, res.Content)
	}
	run(t, Bash{}, s, bashArgs{Command: "false && cd sub", Description: "cd"})
	if s.Cwd != dir {
		t.Fatalf("a cd that never ran moved the tracker to %s", s.Cwd)
	}
	run(t, Bash{}, s, bashArgs{Command: "cd\tsub", Description: "cd"})
	if s.Cwd != filepath.Join(dir, "sub") {
		t.Fatalf("cd with a tab was not followed: %s", s.Cwd)
	}
}

// Without a sandbox, bash is started with no BASH_ENV file to run first and
// no CDPATH to send a cd elsewhere.
func TestBashWithoutSandboxLeavesOutBashEnvAndCdpath(t *testing.T) {
	s, dir := setup(t)
	hook := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hook, []byte("echo HOOK-RAN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", hook)
	t.Setenv("CDPATH", dir)
	res := run(t, Bash{}, s, bashArgs{Command: `echo "[$BASH_ENV][$CDPATH]"`, Description: "env"})
	if strings.Contains(res.Content, "HOOK-RAN") || !strings.Contains(res.Content, "[][]") {
		t.Fatalf("bash kept BASH_ENV or CDPATH: %s", res.Content)
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
