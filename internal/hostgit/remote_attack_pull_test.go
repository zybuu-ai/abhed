package hostgit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A10: a smudge filter planted while pull's fetch is on the network runs on the host at checkout.
func TestAttackFilterPlantedDuringPull(t *testing.T) {
	r := newRemoteRig(t)
	root := filepath.Dir(r.ws)
	seed := filepath.Join(root, "seed")
	os.WriteFile(filepath.Join(seed, "f.txt"), []byte("hello\n"), 0o600)
	run(t, seed, "add", "f.txt")
	run(t, seed, "commit", "-q", "-m", "three")
	run(t, seed, "push", "-q", r.bare, "main")
	// slow network: wrap the person's ssh with a delay
	gc := filepath.Join(r.home, ".gitconfig")
	cfg, _ := os.ReadFile(gc)
	stub := filepath.Join(root, "ssh-stub")
	slow := filepath.Join(root, "ssh-slow")
	os.WriteFile(slow, []byte("#!/bin/sh\nsleep 2\nexec "+stub+" \"$@\"\n"), 0o700)
	os.WriteFile(gc, []byte(strings.Replace(string(cfg), stub, slow, 1)), 0o600)
	b := r.broker(Network{Open: true})
	planted := filepath.Join(r.home, "smudge-ran")
	if _, _, _, err := b.Ask(r.key("c1"), r.ws, "git pull"); err != nil {
		t.Fatal(err)
	}
	cmd := b.Command(r.ctx("c1"), r.ws, "git pull")
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	go func() {
		time.Sleep(700 * time.Millisecond) // the agent's background job, while the fetch runs
		os.WriteFile(filepath.Join(r.ws, ".git", "info", "attributes"), []byte("*.txt filter=evil\n"), 0o600)
		exec.Command("git", "-C", r.ws, "config", "filter.evil.smudge", "sh -c 'touch "+planted+"; cat'").Run()
	}()
	out, err := runCmd(t, cmd)
	t.Logf("pull: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("SMUDGE FILTER PLANTED DURING PULL RAN ON THE HOST")
	}
}
