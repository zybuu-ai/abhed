//go:build darwin

package clitest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// abhed -p, with the model calling bash `git pull`, pulls through the
// person's own git on the host: their SSH command (a stub serving a local
// bare repository) is used, the record has hostgit.brokered, the work tree
// moves, and the session facts say so. The run's home is under /tmp, where
// commands can write, so the person's configuration is given as
// GIT_CONFIG_GLOBAL in a folder of their own home.
func TestHeadlessPullIsBrokered(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("no sandbox-exec: the process tier is not here")
	}
	real, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	person, err := os.MkdirTemp(real, ".abhed-clitest-hostgit-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(person) })
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=a", "-c", "user.email=a@b", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gc := filepath.Join(person, "gitconfig")
	setup := func(_, ws string) {
		root := filepath.Dir(ws)
		remotes := filepath.Join(root, "remotes")
		bare := filepath.Join(remotes, "team", "app.git")
		seed := filepath.Join(root, "seed")
		stubLog := filepath.Join(root, "ssh.log")
		for _, d := range []string{filepath.Dir(bare), seed} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		git(root, "init", "-q", "--bare", bare)
		git(seed, "init", "-q")
		git(seed, "commit", "-q", "--allow-empty", "-m", "one")
		git(seed, "push", "-q", bare, "main")
		git(ws, "init", "-q")
		git(ws, "remote", "add", "origin", "git@git.example:team/app.git")
		git(ws, "fetch", "-q", bare, "main:refs/remotes/origin/main")
		git(ws, "checkout", "-q", "-b", "main", "origin/main")
		git(ws, "config", "branch.main.remote", "origin")
		git(ws, "config", "branch.main.merge", "refs/heads/main")
		git(seed, "commit", "-q", "--allow-empty", "-m", "two")
		git(seed, "push", "-q", bare, "main")
		// The person's ssh is theirs alone: in their own folder, not where commands write.
		stub := filepath.Join(person, "ssh-stub")
		script := "#!/bin/sh\nfor a; do last=$a; done\necho \"host=$1 cmd=$last\" >> " + stubLog + "\ncd " + remotes + " && eval \"$last\"\n"
		if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gc, []byte("[core]\n\tsshCommand = "+stub+"\n[ssh]\n\tvariant = simple\n[user]\n\tname = Person\n\temail = p@example.test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := strings.Replace(DefaultUserConfig, `{"min_tier":"none"}`, `{"min_tier":"process","allow_network":true}`, 1)
	h := piped(t, Opts{UserConfig: cfg, Setup: setup,
		Env:    []string{"GIT_CONFIG_GLOBAL=" + gc, "GIT_CONFIG_NOSYSTEM=1"},
		Args:   []string{"-p", "pull", "-allow", "bash(git*)", "-output-format", "stream-json"},
		Script: "tool bash {\"command\":\"git pull --ff-only\"}\n\ntext \"done\""})
	brokered := false
	for _, e := range ParseEvents(h.Stdout()) {
		if e.Type == "hostgit.brokered" {
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p)
			if p["stage"] == "exited" && p["exit_code"] == float64(0) {
				brokered = true
			}
		}
	}
	if !brokered {
		t.Fatalf("git pull was not brokered:\n%s\n%s", h.Stdout(), h.Stderr())
	}
	// The run's folder is gone once it ends: what it did is in the record.
	if out := h.Stdout(); !strings.Contains(out, "From git.example:team/app") || !strings.Contains(out, "Fast-forward") {
		t.Fatalf("the pull did not fetch through the person's ssh and fast-forward:\n%s", out)
	}
	if req := lastRequestText(t, h); !strings.Contains(req, "through the user's own git on the host") && !strings.Contains(lastRequestRaw(t, h), "through the user's own git on the host") {
		t.Fatalf("the session facts do not say git is brokered:\n%s", req)
	}
}

// lastRequestRaw is the last request to the model as sent.
func lastRequestRaw(t *testing.T, h *Run) string {
	t.Helper()
	reqs := h.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request")
	}
	return string(reqs[len(reqs)-1].Body)
}
