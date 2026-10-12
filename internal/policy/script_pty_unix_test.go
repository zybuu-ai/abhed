//go:build unix

package policy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The row the terminal-line test judges holds for bash itself: typed at a
// terminal's shell, bash runs the .bashrc in its HOME.
func TestBashTypedAtATerminalRunsItsBashrc(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	ran := filepath.Join(home, "ran")
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(": > '"+ran+"'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", "-i")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "TERM=dumb"}
	f, err := pty.Start(cmd)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = f.Close() }()
	go func() { _, _ = drain(f) }()
	_, _ = f.Write([]byte("bash\nexit\nexit\n"))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the shell did not end")
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatal("bash typed at a terminal did not run .bashrc")
	}
}

// drain reads the terminal so the shell never blocks on output.
func drain(f *os.File) (int64, error) {
	buf := make([]byte, 4096)
	var n int64
	for {
		m, err := f.Read(buf)
		n += int64(m)
		if err != nil {
			return n, err
		}
	}
}
