package app

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/ollama"
)

const stopExitsHelper = "ABHED_TEST_STOP_EXITS_HELPER"

// TestOllamaStoppedWhenASignalEndsTheProcess drives the stopExits path, as
// Studio quitting does (SIGTERM, then os.Exit): the stub server this process
// started is stopped, by its own pid.
func TestOllamaStoppedWhenASignalEndsTheProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signals")
	}
	if stub := os.Getenv(stopExitsHelper); stub != "" {
		pid, err := ollama.OwnLaunched(stub, []string{"serve"}, "http://127.0.0.1:1", t.TempDir(), 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		cancelOnStop(stopExits)
		os.Stdout.WriteString("pid " + strconv.Itoa(pid) + "\n")
		select {}
	}
	bin := t.TempDir()
	stub := filepath.Join(bin, "ollama")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestOllamaStoppedWhenASignalEndsTheProcess$")
	cmd.Env = append(os.Environ(), stopExitsHelper+"="+stub)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var server int
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "pid "); ok {
			server, _ = strconv.Atoi(rest)
			break
		}
	}
	if server == 0 {
		t.Fatal("the helper started no stub server")
	}
	// The server is the helper's child: kill it here only if the test fails.
	defer func() { _ = syscall.Kill(-server, syscall.SIGKILL) }()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the process did not exit within the 3 seconds an editor gives it")
	}
	for range 100 {
		if syscall.Kill(server, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the Ollama the process started is still running after it exited on a signal")
}
