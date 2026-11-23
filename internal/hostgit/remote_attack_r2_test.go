package hostgit

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// B1: the person's pre-push dispatches to a work-tree file (.husky/pre-push),
// as common managed hooks do; the agent wrote that file. It must never run
// on the host: it runs in the sandbox, with no agent socket.
func TestAttackPushHookChainsToWorktree(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "managed-hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	disp := "#!/bin/sh\nroot=$(git rev-parse --show-toplevel)\n[ -x \"$root/.husky/pre-push\" ] && exec \"$root/.husky/pre-push\" \"$@\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte(disp), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	planted := filepath.Join(r.home, "husky-ran")
	if err := os.MkdirAll(filepath.Join(r.ws, ".husky"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.ws, ".husky", "pre-push"), []byte("#!/bin/sh\necho SSH_AUTH_SOCK=$SSH_AUTH_SOCK > "+planted+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.inner = nil
	r.mu.Unlock()
	if out, err := runCmd(t, b.Command(r.ctx("c-push"), r.ws, "git push origin main")); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	got, err := os.ReadFile(planted)
	if err != nil {
		t.Fatal("the hook's chain did not run at all")
	}
	if strings.Contains(string(got), sockMarker) {
		t.Fatalf("AGENT-WRITTEN .husky/pre-push RAN WITH THE PERSON'S AGENT SOCKET: %s", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inner) != 1 || !strings.Contains(r.inner[0], filepath.Join(hooks, "pre-push")) {
		t.Fatalf("the hook did not run in the sandbox: %q", r.inner)
	}
}

// B3: a pull's merge runs in the sandbox; nothing but the fetch runs on the host.
func TestAttackPullSplit(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	_, _, _, _ = b.Ask(r.key("p"), r.ws, "git pull --rebase")
	_, _ = runCmd(t, b.Command(r.ctx("p"), r.ws, "git pull --rebase"))
	if len(r.inner) != 1 {
		t.Fatalf("the sandbox ran %q", r.inner)
	}
	for _, l := range r.inner {
		if strings.Contains(l, "fetch") || !strings.Contains(l, "'pull' '--rebase' '.' 'refs/remotes/origin/main'") {
			t.Errorf("sandbox line: %s", l)
		}
	}
}

// B4, and its TLS twin: an http.<url>.* setting in the repository outranks
// the command line's http.proxy and http.sslVerify, so it is refused.
func TestAttackURLScopedHTTP(t *testing.T) {
	for _, plant := range []func(addr string) [2]string{
		func(addr string) [2]string { return [2]string{"http.https://git.example/.proxy", "http://" + addr} },
		func(string) [2]string { return [2]string{"http.https://git.example/.sslVerify", "false"} },
	} {
		r := newRemoteRig(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		var hits atomic.Int32
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				hits.Add(1)
				_ = c.Close()
			}
		}()
		kv := plant(ln.Addr().String())
		run(t, r.ws, "remote", "set-url", "origin", "https://git.example/team/app.git")
		run(t, r.ws, "config", kv[0], kv[1])
		b := r.broker(Network{Open: true})
		if _, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "sets how https is reached") {
			t.Errorf("%s: %v", kv[0], err)
		}
		_, _ = runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
		time.Sleep(200 * time.Millisecond)
		if hits.Load() > 0 {
			t.Errorf("%s: the repository's proxy was reached", kv[0])
		}
		_ = ln.Close()
	}
}
