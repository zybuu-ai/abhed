package app

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A served session (a signed-in account's or a console guest's, which share
// serve's sandbox, or one with a fence of its own) never runs remote git
// through host git as the server's account: git fetch is the sandbox's own
// command, and the model is not told otherwise.
func TestServedSessionsNeverBrokerRemoteGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ws := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "git@git.example:team/app.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cfg := config.Default()
	cfg, sb, bash, err := serveSandbox(cfg, ws)
	if err != nil {
		t.Skipf("no sandbox to serve with here: %v", err)
	}
	defer func() { _ = sandbox.Close(sb) }()
	t.Logf("served on the %s tier, git held: %v", sb.Tier(), sandbox.HoldsGit(sb))
	if bash.HostAsk != nil {
		t.Fatal("a served session's bash brokers remote git")
	}
	// What the host broker would build, as a control: the CLI's on the same sandbox.
	cli := hostgit.NewBroker(ws, sb.Tier, sandboxconfig.HostGitNetwork(cfg), sb.Command).
		HoldsGit(func() bool { return sandbox.HoldsGit(sb) }, func() bool { return sandbox.HoldsNewGit(sb) })
	ctx := context.Background()
	// Host git's process, or the broker's refusal of it: either way, the broker took the line.
	hostLike := func(cmd *exec.Cmd) bool {
		return strings.Contains(strings.Join(cmd.Args, " "), "set -o pipefail") ||
			cmd.Err != nil && strings.HasPrefix(cmd.Err.Error(), "git ")
	}
	if sandbox.HoldsGit(sb) && (sb.Tier() == sandbox.TierProcess || sb.Tier() == sandbox.TierFence) {
		if c := cli.Command(ctx, ws, "git fetch"); !hostLike(c) {
			t.Skipf("the control is not brokered on this host (%v %v)", c.Args, c.Err)
		}
	}
	for _, line := range []string{"git fetch", "git push origin main", "git clone git@git.example:team/app.git c", "git ls-remote origin"} {
		c := bash.Sandbox(ctx, ws, line)
		if hostLike(c) {
			t.Fatalf("%s reached host git on a server: %v", line, c.Args)
		}
		sandbox.Release(c)
	}
	// A session with a fence of its own: the broker it builds, over a stand-in
	// fence that holds git, leaves remote lines to that fence.
	var ran []string
	oldCmd, oldHolds := sessionFenceCommand, sessionFenceHolds
	sessionFenceCommand = func(*sessionFence) func(ctx context.Context, cwd, line string) *exec.Cmd {
		return func(ctx context.Context, _, line string) *exec.Cmd {
			ran = append(ran, line)
			return exec.CommandContext(ctx, "true")
		}
	}
	sessionFenceHolds = func(*sessionFence) func() bool { return func() bool { return true } }
	t.Cleanup(func() { sessionFenceCommand, sessionFenceHolds = oldCmd, oldHolds })
	b, closeFn := fenceSessionBash(cfg, ws)("s1", nil, tools.Bash{})
	defer func() { _ = closeFn() }()
	if b.HostAsk != nil {
		t.Fatal("a served fence session's bash brokers remote git")
	}
	for _, line := range []string{"git fetch", "git push origin main", "git ls-remote origin"} {
		c := b.Sandbox(ctx, ws, line)
		if hostLike(c) {
			t.Fatalf("%s reached host git in a served fence session: %v %v", line, c.Args, c.Err)
		}
	}
	if len(ran) != 3 {
		t.Fatalf("the fence ran %q", ran)
	}
}

// unchosen stands in for the CLI's sandbox while it is still being chosen.
type unchosen struct{ sandbox.Sandbox }

func (unchosen) Resolved() bool { return false }

// While the CLI's sandbox is still being chosen, bash's description answers
// as the process tier, the floor, would, without waiting for the choice.
func TestCLIHostGitOnUsesTheFloorWhileChoosing(t *testing.T) {
	gb := hostgit.NewBroker(t.TempDir(), func() sandbox.Tier { return sandbox.TierProcess }, hostgit.Network{}, nil)
	cfg := config.Default()
	if !cliHostGitOn(cfg, unchosen{}, gb) {
		t.Fatal("with git protected by default, the floor brokers")
	}
	off := false
	cfg.Sandbox.GitProtect = &off
	if cliHostGitOn(cfg, unchosen{}, gb) {
		t.Fatal("with protect_git off, the floor does not broker")
	}
	if cliHostGitOn(config.Default(), unchosen{}, gb.RemotesOff("sandbox.host_git is false")) {
		t.Fatal("turned off, it does not broker")
	}
}
