package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/internal/fence/mountns"
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

// Missing, the workspace's .abhed is made to mount over, and stays, empty,
// after Close: another fence on the workspace may still mount over it.
func TestFenceMountsMakesAndKeepsTheStateFolder(t *testing.T) {
	p := fencePolicy(t)
	p.MaxProcs, p.MaxMemoryMB = 64, 256
	f := selectFence(t, p)
	if f.Mode() != FenceModeMounts || !f.madeState {
		t.Fatalf("mode %s, made %v", f.Mode(), f.madeState)
	}
	if out, err := fenceRun(t, f, p.Workspace, "echo x > .abhed/x", nil, "call-x"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(filepath.Join(p.Workspace, ".abhed"))
	if err != nil || len(left) != 0 {
		t.Fatalf("the made .abhed after Close: %v %v", err, left)
	}
}

// selectFence selects the fence for p, or skips; the caller closes it.
func selectFence(t *testing.T, p Policy) *Fence {
	t.Helper()
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 in a delegated cgroup to fence real commands")
	}
	sb, err := Select(p)
	if err != nil {
		t.Fatalf("the fence did not qualify: %v", err)
	}
	f := sb.(*Fence)
	if f.Mode() != FenceModeMounts {
		t.Fatalf("the fence is in mode %s", f.Mode())
	}
	return f
}

// Two fences on one workspace, as two Studio tabs or Studio beside the
// command line: one closing while the other's command runs leaves that
// command's .abhed covered, so what it writes there does not persist, and
// the other's next command runs.
func TestFenceMountsTwoFencesOnOneWorkspace(t *testing.T) {
	p := fencePolicy(t)
	p.MaxProcs, p.MaxMemoryMB = 64, 256
	a := selectFence(t, p)
	t.Cleanup(func() { _ = a.Close() })
	b := selectFence(t, p)
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("closing the second fence: %v", err)
		}
	})
	if !a.madeState || b.madeState {
		t.Fatalf("made: first %v, second %v", a.madeState, b.madeState)
	}
	ws := p.Workspace
	ev := &events{}
	started := make(chan struct{})
	var once sync.Once
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := fenceRun(t, b, ws, `sleep 3; mkdir -p .abhed && echo '{"users":[{"name":"mallory","role":"admin"}]}' > .abhed/users.json && ls -A .abhed`,
			ev.record(func() { once.Do(func() { close(started) }) }), "call-long")
		done <- result{out, err}
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the second fence's command did not launch")
	}
	time.Sleep(500 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("closing the first fence: %v", err)
	}
	r := <-done
	t.Logf("the long command: %v %s", r.err, strings.TrimSpace(r.out))
	if r.err != nil || !strings.Contains(r.out, "users.json") {
		t.Fatalf("the long command: %v\n%s", r.err, r.out)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".abhed", "users.json")); err == nil {
		t.Fatal("users.json the command wrote persists in the workspace")
	}
	out, err := fenceRun(t, b, ws, "echo next; ls -A .abhed | wc -l", ev.record(nil), "call-next")
	if err != nil || !strings.HasPrefix(out, "next") {
		t.Fatalf("the next command: %v\n%s", err, out)
	}
	if b.planted.Load() != nil || len(ev.of(EvFenceStatePlanted)) != 0 {
		t.Fatalf("the second fence was marked planted: %v %v", b.planted.Load(), ev.of(EvFenceStatePlanted))
	}
	if left, err := os.ReadDir(filepath.Join(ws, ".abhed")); err != nil || len(left) != 0 {
		t.Fatalf("the workspace's .abhed: %v %v", err, left)
	}
}

// A protected file with a second name, a hard link elsewhere in the
// workspace, refuses the command: held read-only by one name, it would stay
// writable through the other.
func TestFenceMountsRefusesAHardLinkedProtectedFile(t *testing.T) {
	var ws string
	f, _ := mounted(t, func(p *Policy) {
		ws = p.Workspace
		if err := os.MkdirAll(filepath.Join(ws, ".vscode"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".vscode", "settings.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		p.WriteProtected = []string{filepath.Join(ws, ".vscode", "settings.json")}
	})
	ev := &events{}
	if out, err := fenceRun(t, f, ws, "echo ok", ev.record(nil), "call-before"); err != nil {
		t.Fatalf("before the link: %v %s", err, out)
	}
	if err := os.Link(filepath.Join(ws, ".vscode", "settings.json"), filepath.Join(ws, "settings-link.json")); err != nil {
		t.Fatal(err)
	}
	out, err := fenceRun(t, f, ws, `echo '{"x":1}' > settings-link.json`, ev.record(nil), "call-link")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 126 || !strings.Contains(out, "hard links") {
		t.Fatalf("a hard-linked protected file: %v\n%s", err, out)
	}
	t.Logf("refused: %s", strings.TrimSpace(out))
	if b, _ := os.ReadFile(filepath.Join(ws, ".vscode", "settings.json")); string(b) != "{}" {
		t.Fatalf("settings.json changed: %q", b)
	}
}

const threadCapsEnv = "ABHED_TEST_THREAD_CAPS"

// Started by TestFenceCapabilityCheckReadsTheExecutingThread in a user
// namespace holding CAP_SYS_ADMIN, as the launcher is.
func init() {
	if os.Getenv(threadCapsEnv) != "1" {
		return
	}
	done := make(chan int)
	// A goroutine other than main's, which init keeps on the leader.
	go func() {
		runtime.LockOSThread()
		if unix.Gettid() == os.Getpid() {
			fmt.Println("on the leader thread")
			done <- 3
			return
		}
		if err := mountns.Drop(); err != nil {
			fmt.Println(err)
			done <- 1
			return
		}
		if err := holdsNothing(); err != nil {
			fmt.Println(err)
			done <- 1
			return
		}
		// The leader still holds it, so reading its status would refuse.
		leader, _ := os.ReadFile("/proc/self/status")
		if !strings.Contains(string(leader), "CapEff:\t") || strings.Contains(string(leader), "CapEff:\t0000000000000000") {
			fmt.Printf("the leader holds no capability, so the test proves nothing:\n%s", leader)
			done <- 2
			return
		}
		fmt.Println("checked the thread that dropped them")
		done <- 0
	}()
	os.Exit(<-done)
}

// The launcher checks the capabilities of the thread that executes the
// command, not the leader's, which may still hold the mounts' capability.
func TestFenceCapabilityCheckReadsTheExecutingThread(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 where an ordinary user can make a user namespace")
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^$")
	cmd.Env = append(os.Environ(), threadCapsEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	mountns.Attr(cmd.SysProcAttr)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// A skill's script in ~/.abhed/skills or in a skills.dirs folder runs under
// the fence, and cannot be changed from it; the rest of ~/.abhed stays out
// of reach.
func TestFenceRunsASkillScript(t *testing.T) {
	for _, mode := range []func(*Policy){nil, noMounts} {
		var home, script, team string
		f, p := fenced(t, func(p *Policy) {
			home, _ = os.UserHomeDir()
			script = filepath.Join(home, ".abhed", "skills", "hello", "run.sh")
			team = filepath.Join(t.TempDir(), "team", "hello", "run.sh")
			for _, s := range []string{script, team} {
				if err := os.MkdirAll(filepath.Dir(s), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(s, []byte("#!/bin/sh\necho skill ran\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			p.SkillDirs = []string{filepath.Dir(filepath.Dir(team))}
			if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte("abhed-state"), 0o600); err != nil {
				t.Fatal(err)
			}
			if mode != nil {
				mode(p)
			}
		})
		ev := &events{}
		for _, s := range []string{script, team} {
			out, err := fenceRun(t, f, p.Workspace, s, ev.record(nil), "call-skill")
			if err != nil || strings.TrimSpace(out) != "skill ran" {
				t.Fatalf("%s: the skill's script %s: %v\n%s", f.Mode(), s, err, out)
			}
		}
		for _, c := range []string{"echo x >> " + script, "touch " + filepath.Join(filepath.Dir(script), "new"), "cat " + filepath.Join(home, ".abhed", "config.json"),
			"echo x >> " + team, "touch " + filepath.Join(filepath.Dir(team), "new")} {
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
