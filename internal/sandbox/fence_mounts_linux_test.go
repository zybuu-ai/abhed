package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/zybuu-ai/abhed/internal/fence/probe"
)

// These need a host that gives an ordinary user a user namespace, as well as
// what the other fence tests need, and run with ABHED_REQUIRE_FENCE=1.

// mounted is fenced, failing unless the fence chose the mount mode.
func mounted(t *testing.T, mutate func(*Policy)) (*Fence, Policy) {
	t.Helper()
	f, p := fenced(t, mutate)
	if f.Mode() != FenceModeMounts {
		c, _ := f.Report().Check(probe.CheckMounts)
		t.Fatalf("the fence is in mode %s: %s", f.Mode(), c.Reason)
	}
	return f, p
}

func gitInit(t *testing.T, ws string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "first"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// Under ProtectGit and a write-protected file, a fenced command cannot write
// git's hooks or config, move the git folder away, or write the protected
// file, from the workspace or from inside the hooks folder; it can commit.
func TestFenceMountsProtectGit(t *testing.T) {
	var ws string
	f, p := mounted(t, func(p *Policy) {
		ws = p.Workspace
		gitInit(t, ws)
		if err := os.MkdirAll(filepath.Join(ws, ".vscode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".vscode", "settings.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		p.ProtectGit = true
		p.WriteProtected = []string{filepath.Join(ws, ".vscode", "settings.json")}
	})
	config, _ := os.ReadFile(filepath.Join(ws, ".git", "config"))
	ev := &events{}
	for _, c := range []string{
		"echo x > .git/hooks/x",
		"echo '[core]' >> .git/config",
		"git config user.name mallory",
		"mv .git .git-away",
		"rm -rf .git/hooks",
		"echo x > .vscode/settings.json",
		"mv .vscode .vscode-away",
		// Abhed's own view of the files, past the command's mounts.
		"echo x > /proc/$PPID/root" + ws + "/.git/hooks/x",
		"echo x > /proc/1/root" + ws + "/.git/hooks/x",
	} {
		out, err := fenceRun(t, f, ws, c, ev.record(nil), "call-deny")
		if err == nil {
			t.Errorf("%q succeeded:\n%s", c, out)
		}
		t.Logf("%s: %v: %s", c, err, strings.TrimSpace(out))
	}
	if out, err := fenceRun(t, f, filepath.Join(ws, ".git", "hooks"), "touch y", ev.record(nil), "call-cwd"); err == nil {
		t.Errorf("a write from inside the hooks folder succeeded:\n%s", out)
	}
	commit := "echo hi > a.txt && git add a.txt && git -c user.name=t -c user.email=t@example.com commit -q -m second && git log --oneline | wc -l"
	out, err := fenceRun(t, f, ws, commit, ev.record(nil), "call-commit")
	if err != nil || strings.TrimSpace(out) != "2" {
		t.Fatalf("commit under the fence: %v\n%s", err, out)
	}
	for _, x := range []string{".git/hooks/x", ".git/hooks/y", ".git-away", ".vscode-away"} {
		if _, err := os.Lstat(filepath.Join(ws, x)); err == nil {
			t.Errorf("%s exists", x)
		}
	}
	if now, _ := os.ReadFile(filepath.Join(ws, ".git", "config")); string(now) != string(config) {
		t.Errorf(".git/config changed:\n%s", now)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".vscode", "settings.json")); string(b) != "{}" {
		t.Errorf("settings.json changed: %q", b)
	}
	for _, l := range ev.of("process.launched") {
		if l["mode"] != FenceModeMounts || !strings.Contains(l["mounts"].(string), "read-only") {
			t.Errorf("process.launched: %v", l)
		}
	}
	q := f.Qualification()
	if q["mode"] != FenceModeMounts || q["protected"] == nil {
		t.Errorf("fence.qualified: mode %v protected %v", q["mode"], q["protected"])
	}
	_ = p
}

// The workspace's .abhed is covered by an empty tmpfs in each command: what
// Abhed keeps there is out of sight, and what a command writes there is gone
// when it ends, so nothing is planted and the session goes on.
func TestFenceMountsStateDoesNotPersist(t *testing.T) {
	var ws string
	f, _ := mounted(t, func(p *Policy) {
		ws = p.Workspace
		if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(`{"secret":"abhed-state"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	ev := &events{}
	out, err := fenceRun(t, f, ws, `cat .abhed/config.json; ls -A .abhed; echo '{"users":[{"name":"mallory"}]}' > .abhed/users.json && cat .abhed/users.json`, ev.record(nil), "call-plant")
	if err != nil || strings.Contains(out, "abhed-state") || !strings.Contains(out, "mallory") {
		t.Fatalf("inside the fence: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, ".abhed", "users.json")); err == nil {
		t.Fatal("users.json written inside the fence persists")
	}
	if out, err := fenceRun(t, f, ws, "cat /proc/$PPID/root"+ws+"/.abhed/config.json || cat /proc/$PPID/cwd/.abhed/config.json", ev.record(nil), "call-proc"); err == nil || strings.Contains(out, "abhed-state") {
		t.Fatalf("Abhed's view of the workspace was reached: %v\n%s", err, out)
	} else {
		t.Logf("through /proc: %v: %s", err, strings.TrimSpace(out))
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".abhed", "config.json")); !strings.Contains(string(b), "abhed-state") {
		t.Fatalf("Abhed's own state changed: %q", b)
	}
	if out, err := fenceRun(t, f, ws, "echo next", ev.record(nil), "call-next"); err != nil || f.planted.Load() != nil {
		t.Fatalf("the session did not go on: %v %s", err, out)
	}
	if got := ev.of(EvFenceStatePlanted); len(got) != 0 {
		t.Fatalf("fence.state_planted: %v", got)
	}
	// Another spelling is not covered, and is still taken out.
	if out, err := fenceRun(t, f, ws, "mkdir .ABHED", ev.record(nil), "call-other"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	eventually(t, "another spelling of .abhed taken out", 10*time.Second, func() bool {
		return f.planted.Load() != nil && len(ev.of(EvFenceStatePlanted)) == 1
	})
}

// Missing, the workspace's .abhed is made to mount over and removed at Close.
func TestFenceMountsMakesAndRemovesTheStateFolder(t *testing.T) {
	p := fencePolicy(t)
	p.MaxProcs, p.MaxMemoryMB = 64, 256
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	sb, err := Select(p)
	if err != nil {
		t.Fatal(err)
	}
	f := sb.(*Fence)
	if f.Mode() != FenceModeMounts || !f.madeState {
		t.Fatalf("mode %s, made %v", f.Mode(), f.madeState)
	}
	if out, err := fenceRun(t, f, p.Workspace, "echo x > .abhed/x", nil, "call-x"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p.Workspace, ".abhed")); !os.IsNotExist(err) {
		t.Fatalf("the .abhed the fence made outlived Close: %v", err)
	}
}

// A skill's script in ~/.abhed/skills runs under the fence, and cannot be
// changed from it; the rest of ~/.abhed stays out of reach.
func TestFenceRunsASkillScript(t *testing.T) {
	for _, mode := range []func(*Policy){nil, noMounts} {
		var home, script string
		f, p := fenced(t, func(p *Policy) {
			home, _ = os.UserHomeDir()
			script = filepath.Join(home, ".abhed", "skills", "hello", "run.sh")
			if err := os.MkdirAll(filepath.Dir(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(script, []byte("#!/bin/sh\necho skill ran\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte("abhed-state"), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode != nil {
				mode(p)
			}
		})
		ev := &events{}
		out, err := fenceRun(t, f, p.Workspace, script, ev.record(nil), "call-skill")
		if err != nil || strings.TrimSpace(out) != "skill ran" {
			t.Fatalf("%s: the skill's script: %v\n%s", f.Mode(), err, out)
		}
		for _, c := range []string{"echo x >> " + script, "touch " + filepath.Join(filepath.Dir(script), "new"), "cat " + filepath.Join(home, ".abhed", "config.json")} {
			if out, err := fenceRun(t, f, p.Workspace, c, ev.record(nil), "call-deny"); err == nil || strings.Contains(out, "abhed-state") {
				t.Errorf("%s: %q succeeded:\n%s", f.Mode(), c, out)
			}
		}
	}
}

// Where user namespaces are not allowed, run with ABHED_FENCE_NO_USERNS=1:
// the probe says so, a surface with protected paths is refused, naming why,
// and the command line's fence runs in the landlock_only mode, saying so.
func TestFenceWithoutUserNamespaces(t *testing.T) {
	if os.Getenv("ABHED_FENCE_NO_USERNS") != "1" {
		t.Skip("set ABHED_FENCE_NO_USERNS=1 where an ordinary user cannot make a user namespace")
	}
	for name, set := range map[string]func(*Policy){
		"protect git":     func(p *Policy) { p.ProtectGit = true },
		"write protected": func(p *Policy) { p.WriteProtected = []string{filepath.Join(p.Workspace, ".vscode")} },
	} {
		p := fencePolicy(t)
		set(&p)
		sb, err := Select(p)
		if err == nil {
			_ = Close(sb)
			t.Fatalf("%s: selected %s", name, sb.Tier())
		}
		if !strings.Contains(err.Error(), "read-only area") || !strings.Contains(err.Error(), "no other tier") || !strings.Contains(err.Error(), "mount namespace") {
			t.Errorf("%s: %v", name, err)
		}
		t.Logf("%s: %v", name, err)
	}
	f, p := fenced(t, nil)
	if f.Mode() != FenceModeLandlock {
		t.Fatalf("mode %s", f.Mode())
	}
	c, _ := f.Report().Check(probe.CheckMounts)
	if c.Status != probe.Fail || f.Qualification()["mounts_unavailable"] == nil {
		t.Fatalf("the probe's mounts check: %+v", c)
	}
	t.Logf("userns_mounts: %s", c.Reason)
	ev := &events{}
	if out, err := fenceRun(t, f, p.Workspace, "echo hi", ev.record(nil), "call-hi"); err != nil || strings.TrimSpace(out) != "hi" {
		t.Fatalf("%v %s", err, out)
	}
	if l := ev.of("process.launched"); len(l) != 1 || l[0]["mode"] != FenceModeLandlock || l[0]["mounts"] != "none" {
		t.Fatalf("process.launched: %v", l)
	}
}

// The workbench terminal's shell starts on a terminal of its own under the
// fence in its mount mode, and cannot write git's hooks.
func TestFenceMountsShellOnATerminal(t *testing.T) {
	var ws string
	f, _ := mounted(t, func(p *Policy) {
		ws = p.Workspace
		gitInit(t, ws)
		p.ProtectGit = true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := f.Shell(WithLaunch(ctx, Launch{CallID: "call-shell"}), ws)
	tty, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	_, _ = tty.WriteString("echo x > .git/hooks/x; echo hooks=$?; echo $((6*7)); exit\n")
	var out strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(out.String(), "42") {
		n, err := tty.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = cmd.Wait()
	if !strings.Contains(out.String(), "42") || !strings.Contains(out.String(), "hooks=1") {
		t.Fatalf("shell output:\n%s", out.String())
	}
	if _, err := os.Lstat(filepath.Join(ws, ".git", "hooks", "x")); err == nil {
		t.Fatal("the shell wrote a hook")
	}
}
