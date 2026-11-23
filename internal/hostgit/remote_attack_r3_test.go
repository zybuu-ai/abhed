package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// C1: a push cancelled while the person's pre-push runs leaves host processes behind?
func TestR3CancelDuringPrePush(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	os.MkdirAll(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nsleep 8\nexit 0\n"), 0o755)
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	ctx, cancel := context.WithTimeout(r.ctx("c-push"), 2*time.Second)
	defer cancel()
	start := time.Now()
	out, err := runCmd(t, b.Command(ctx, r.ws, "git push origin main"))
	t.Logf("push after %v: %v\n%s", time.Since(start), err, out)
	time.Sleep(500 * time.Millisecond)
	ps, _ := exec.Command("ps", "-axo", "pid,ppid,command").Output()
	var left []string
	for _, l := range strings.Split(string(ps), "\n") {
		if strings.Contains(l, filepath.Dir(r.ws)) && !strings.Contains(l, "ps -axo") {
			left = append(left, l)
		}
	}
	t.Logf("left after cancel:\n%s", strings.Join(left, "\n"))
	// Ended with the cancel, not when the hook would have.
	time.Sleep(time.Second)
	ps, _ = exec.Command("ps", "-axo", "pid,ppid,command").Output()
	for _, l := range strings.Split(string(ps), "\n") {
		if (strings.Contains(l, "git-receive-pack") || strings.Contains(l, "git push") || strings.Contains(l, "ssh-stub")) && strings.Contains(l, filepath.Dir(r.ws)) {
			t.Errorf("LEFT 1.5s AFTER THE CANCEL: %s", l)
		}
	}
	time.Sleep(9 * time.Second)
	ps, _ = exec.Command("ps", "-axo", "pid,ppid,command").Output()
	var still []string
	for _, l := range strings.Split(string(ps), "\n") {
		if (strings.Contains(l, "pre-push") || strings.Contains(l, "git-receive-pack") || strings.Contains(l, "git push")) && strings.Contains(l, filepath.Dir(r.ws)) {
			still = append(still, l)
		}
	}
	if len(still) > 0 {
		t.Errorf("PROCESSES LEFT 9s AFTER THE HOOK SHOULD HAVE ENDED:\n%s", strings.Join(still, "\n"))
	}
	log, _ := os.ReadFile(r.sshLog)
	t.Logf("ssh log:\n%s", log)
	t.Logf("remote main=%s local=%s", run(t, r.bare, "rev-parse", "main"), run(t, r.ws, "rev-parse", "main"))
}

// C2: nothing to push: the relay is never asked; the push must not hang.
func TestR3NothingToPush(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	os.MkdirAll(hooks, 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	runCmd(t, b.Command(r.ctx("c1"), r.ws, "git push origin main"))
	start := time.Now()
	out, err := runCmd(t, b.Command(r.ctx("c2"), r.ws, "git push origin main"))
	t.Logf("second push %v: %v\n%s", time.Since(start), err, out)
	if time.Since(start) > 5*time.Second {
		t.Errorf("an up-to-date push took %v", time.Since(start))
	}
}

// C3: a ref name with shell metacharacters reaches the relayed hook's heredoc intact (sandbox side only).
func TestR3RefNameInHeredoc(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	os.MkdirAll(hooks, 0o755)
	got := filepath.Join(r.home, "hook-input")
	os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\ncat > "+got+"\nexit 0\n"), 0o755)
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	run(t, r.ws, "branch", "x$(touch${IFS}PWN)`id`ABHED_PRE_PUSH'")
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git push origin main"))
	t.Logf("push: %v\n%s", err, out)
	b2, _ := os.ReadFile(got)
	t.Logf("hook input: %q", b2)
}
