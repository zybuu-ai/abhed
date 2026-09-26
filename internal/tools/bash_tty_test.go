//go:build unix

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// ttyHelper, run under a terminal by the test below, runs one command the way
// the CLI does and prints how it ended.
func TestTTYHelper(t *testing.T) {
	if os.Getenv("ABHED_TTY_HELPER") == "" {
		t.Skip("run by TestBashDevTTYReadFailsFast")
	}
	s, _ := setup(t)
	args, _ := json.Marshal(bashArgs{Command: `read x < /dev/tty; echo "rc=$?"`, Description: "tty", TimeoutMS: 5000})
	start := time.Now()
	res := Bash{}.Run(context.Background(), s, args)
	fmt.Printf("RESULT took=%s %q\n", time.Since(start).Round(time.Millisecond), res.Content)
}

// A command that reads the terminal, as a password prompt does, fails at
// once: it has no terminal. Stopped in a background group, it hung unseen.
func TestBashDevTTYReadFailsFast(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTTYHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "ABHED_TTY_HELPER=1")
	tty, err := pty.Start(cmd) // the helper leads a session with this terminal as its own
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer tty.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(tty)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("the helper ended without a result")
			}
			if !strings.HasPrefix(line, "RESULT ") {
				continue
			}
			if strings.Contains(line, "timed out") || strings.Contains(line, "rc=0") || !strings.Contains(line, "rc=") {
				t.Fatalf("a /dev/tty read did not fail fast: %s", line)
			}
			return
		case <-deadline:
			t.Fatal("no result from the helper")
		}
	}
}
