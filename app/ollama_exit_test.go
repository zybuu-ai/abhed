package app

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/ollama"
)

const stopHelper = "ABHED_TEST_STOP_HELPER"

// stubOllama is a stand-in server that notes a TERM in marker and ends on
// its own after ten seconds, so no test ever has to signal it.
func stubOllama(t *testing.T) (path, marker string) {
	dir := t.TempDir()
	path, marker = filepath.Join(dir, "ollama"), filepath.Join(dir, "stopped")
	// sleep runs in the background and is waited for, so the trap runs at once.
	script := "#!/bin/sh\ntrap 'echo stopped > " + marker + "; exit 0' TERM\n: > " + path + ".up\ni=0\nwhile [ $i -lt 100 ]; do sleep 0.1 & wait $!; i=$((i+1)); done\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, marker
}

// awaitStub waits until the stub has set its trap.
func awaitStub(stub string) {
	waitFile(stub+".up", 5*time.Second)
}

// runStopHelper runs this test binary as a helper in mode, with the stub, and
// signals the helper (its own child) once it says it is ready.
func runStopHelper(t *testing.T, test, mode, stub string) (lines []string, exited bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), stopHelper+"="+mode+":"+stub)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "ready" {
			break
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		exited = true
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill() // the helper, this test's own child
		<-done
	}
	return lines, exited
}

func stopHelperMode() (mode, stub string, ok bool) {
	mode, stub, ok = strings.Cut(os.Getenv(stopHelper), ":")
	return
}

func waitFile(path string, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// TestOllamaStoppedWhenASignalEndsTheProcess drives the stopExits path, as
// Studio quitting does (SIGTERM, then os.Exit): the server the process
// started is stopped by it, within the 3 seconds an editor gives.
func TestOllamaStoppedWhenASignalEndsTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	if mode, stub, ok := stopHelperMode(); ok && mode == "exits" {
		if _, err := ollama.OwnLaunchedForTests(stub, []string{"serve"}, "http://127.0.0.1:1", t.TempDir(), 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		cancelOnStop(stopExits)
		awaitStub(stub)
		os.Stdout.WriteString("ready\n")
		select {}
	}
	stub, marker := stubOllama(t)
	if _, exited := runStopHelper(t, "TestOllamaStoppedWhenASignalEndsTheProcess", "exits", stub); !exited {
		t.Fatal("the process did not exit within the 3 seconds an editor gives it")
	}
	if !waitFile(marker, 2*time.Second) {
		t.Fatal("the Ollama the process started was not stopped when it exited on a signal")
	}
}

// TestOllamaKeptWhileServeDrains: a drain (abhed serve's SIGTERM) leaves the
// server it started running for the sessions still draining.
func TestOllamaKeptWhileServeDrains(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	if mode, stub, ok := stopHelperMode(); ok && mode == "drains" {
		_, marker, _ := strings.Cut(stub, "|")
		stub, _, _ = strings.Cut(stub, "|")
		if _, err := ollama.OwnLaunchedForTests(stub, []string{"serve"}, "http://127.0.0.1:1", t.TempDir(), 10*time.Minute); err != nil {
			t.Fatal(err)
		}
		s := cancelOnStop(stopDrains)
		awaitStub(stub)
		os.Stdout.WriteString("ready\n")
		<-s.ctx.Done()
		time.Sleep(500 * time.Millisecond)
		if _, err := os.Stat(marker); err == nil {
			os.Stdout.WriteString("stopped during the drain\n")
		} else {
			os.Stdout.WriteString("kept\n")
		}
		s.stop()
		ollama.StopOwned("test end")
		os.Exit(0)
	}
	stub, marker := stubOllama(t)
	lines, _ := runStopHelper(t, "TestOllamaKeptWhileServeDrains", "drains", stub+"|"+marker)
	if !strings.Contains(strings.Join(lines, "\n"), "kept") {
		t.Fatalf("the server was not kept while draining: %q", lines)
	}
}
