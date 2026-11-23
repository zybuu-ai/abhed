package hostgit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// Only a whole, plain worktree or submodule command is brokered.
func TestBrokeredLines(t *testing.T) {
	for line, want := range map[string]bool{
		"git worktree add -b x wt HEAD":        true,
		"git worktree remove --force wt":       true,
		"git worktree prune":                   true,
		"git submodule update --init":          false,
		"git worktree add wt; rm -rf /":        false,
		"git worktree add $(evil)":             false,
		"git worktree add --reason=x wt":       false,
		"git status":                           false,
		"git -c core.fsmonitor=x worktree add": false,
		"git submodule add https://x/y.git y":  false,
	} {
		if _, got := brokered(line); got != want {
			t.Errorf("%q: brokered %v", line, got)
		}
	}
}

// Under the process tier, git worktree add and remove run on the host and
// work; a path outside the workspace is refused; other lines go to the sandbox.
func TestBrokerRunsWorktreesOnTheHost(t *testing.T) {
	ws := gitRepo(t)
	t.Setenv("HOME", t.TempDir())
	if out, err := exec.Command("sh", "-c", "cd "+ws+" && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m one").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var seen []string
	inner := func(ctx context.Context, cwd, line string) *exec.Cmd {
		seen = append(seen, line)
		return exec.CommandContext(ctx, "true")
	}
	process := func() sandbox.Tier { return sandbox.TierProcess }
	run := Broker(ws, process, true, inner)
	ctx := context.Background()
	if out, err := run(ctx, ws, "git worktree add -q -b w1 wt HEAD").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, "wt", ".git")); err != nil {
		t.Fatalf("no worktree: %v", err)
	}
	if out, err := run(ctx, ws, "git worktree remove --force wt").CombinedOutput(); err != nil {
		t.Fatalf("worktree remove: %v %s", err, out)
	}
	if err := run(ctx, ws, "git worktree add "+t.TempDir()+"/out").Run(); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("a worktree outside the workspace: %v", err)
	}
	_ = run(ctx, ws, "git status").Run()
	if len(seen) != 1 || seen[0] != "git status" {
		t.Fatalf("the sandbox saw %v", seen)
	}
	// The container tier is left to its sandbox.
	container := func() sandbox.Tier { return sandbox.TierContainer }
	_ = Broker(ws, container, true, inner)(ctx, ws, "git worktree prune").Run()
	if len(seen) != 2 {
		t.Fatalf("a container tier's line was brokered: %v", seen)
	}
}

func lastEnv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// Remote operations are brokered only on the process tier holding git's
// configuration, and the facts line says so from the broker's own rule.
func TestBrokersRemotes(t *testing.T) {
	ws := t.TempDir()
	if !BrokersRemotes(sandbox.NewProcess(sandbox.Policy{Workspace: ws, ProtectGit: true})) {
		t.Fatal("a process tier holding git is not reported brokered")
	}
	for _, sb := range []sandbox.Sandbox{nil, sandbox.NewProcess(sandbox.Policy{Workspace: ws}), sandbox.NewNone(sandbox.Policy{Workspace: ws})} {
		if BrokersRemotes(sb) {
			t.Fatalf("remote operations reported brokered for %v", sb)
		}
	}
	if !Brokered("git worktree add ../x") {
		t.Fatal("the probe's own path is not the broker's")
	}
}

// git submodule update is never run on the host: the submodules' own
// configuration and attributes name programs (a smudge filter, an update
// command) host git would run as the person, or as a server's account.
// The line goes to the sandbox, whatever the network and on a server too.
func TestSubmoduleUpdateIsNeverBrokered(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "protocol.file.allow=always", "-c", "user.name=a", "-c", "user.email=a@b"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	sub, ws := filepath.Join(root, "sub"), filepath.Join(root, "ws")
	for _, d := range []string{sub, ws, filepath.Join(root, "home")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git(sub, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("*.txt filter=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(sub, "add", ".")
	git(sub, "commit", "-q", "-m", "one")
	git(ws, "init", "-q", "-b", "main")
	git(ws, "submodule", "add", "-q", sub, "sub")
	git(ws, "commit", "-q", "-m", "sub")
	git(ws, "submodule", "deinit", "-q", "-f", "sub")
	marker := filepath.Join(root, "pwn")
	git(ws, "config", "submodule.sub.update", "!touch "+marker)
	git(filepath.Join(ws, ".git", "modules", "sub"), "config", "filter.evil.smudge", "sh -c 'touch "+marker+"-smudge; cat'")
	var inner []string
	sandboxCmd := func(ctx context.Context, _, line string) *exec.Cmd {
		inner = append(inner, line)
		return exec.CommandContext(ctx, "true")
	}
	process := func() sandbox.Tier { return sandbox.TierProcess }
	ctx := context.Background()
	for _, b := range []*HostBroker{
		NewBroker(ws, process, Network{Open: true}, sandboxCmd),
		NewBroker(ws, process, Network{}, sandboxCmd),
		NewBroker(ws, process, Network{Open: true}, sandboxCmd).NoRemotes(),
	} {
		for _, line := range []string{"git submodule update --init --no-fetch", "git submodule update --init", "git submodule update -N"} {
			if Brokered(line) {
				t.Fatalf("%q is brokered", line)
			}
			if err := b.Command(ctx, ws, line).Run(); err != nil {
				t.Fatalf("%q: %v", line, err)
			}
		}
	}
	if len(inner) != 9 {
		t.Fatalf("the sandbox ran %q", inner)
	}
	for _, m := range []string{marker, marker + "-smudge"} {
		if _, err := os.Stat(m); err == nil {
			t.Fatalf("a submodule's program ran on the host (%s)", m)
		}
	}
}
