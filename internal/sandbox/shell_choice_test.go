package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The terminal's shell is the person's zsh where that is their login shell,
// else the newest bash outside home; one inside home is never chosen.
func TestChooseShell(t *testing.T) {
	if _, err := os.Stat("/bin/zsh"); err == nil {
		t.Setenv("SHELL", "/bin/zsh")
		if c := chooseShell(); !c.zsh || c.line != zshLine {
			t.Fatalf("zsh: %+v", c)
		}
	}
	t.Setenv("SHELL", "/bin/bash")
	if c := chooseShell(); c.zsh || c.line != TerminalCommand || !strings.HasSuffix(c.argv[0], "bash") {
		t.Fatalf("bash: %+v", c)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	planted := filepath.Join(home, "bin", "zsh")
	_ = os.MkdirAll(filepath.Dir(planted), 0o755)
	_ = os.WriteFile(planted, []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("SHELL", planted)
	if c := chooseShell(); c.zsh {
		t.Fatalf("a shell in home was chosen: %+v", c)
	}
}

// A zsh terminal on the macOS process tier runs Abhed's start-up file, which
// no command may change, with a bash-like prompt; it is recorded as zsh.
func TestZshTerminal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("zsh terminals are the macOS process tier's")
	}
	if _, err := os.Stat("/bin/zsh"); err != nil {
		t.Skip("no zsh")
	}
	old := hostShell
	t.Cleanup(func() { hostShell = old })
	hostShell = func() shellChoice {
		return shellChoice{argv: []string{"/bin/zsh", "-d", "-i"}, line: zshLine, zsh: true}
	}
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	defer func() { _ = s.Close() }()
	if s.ShellLine() != zshLine || TerminalCommandOf(s) != zshLine {
		t.Fatalf("line %q", s.ShellLine())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	z := s.zdotdir.Load()
	cmd := s.Shell(ctx, ws)
	cmd.Stdin = strings.NewReader(`echo "zsh-$ZSH_VERSION"; echo x >> "` + filepath.Join(*z, ".zshrc") + `" 2>/dev/null || echo rc-protected` + "\nexit\n")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "zsh-5") || !strings.Contains(string(out), "rc-protected") || !strings.Contains(string(out), "$ ") {
		t.Fatalf("%q", out)
	}
}
