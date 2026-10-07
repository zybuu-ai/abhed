package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These run with ABHED_REQUIRE_FENCE=1 on a host that allows the mount mode,
// and mirror the git protection the process and container tiers give.

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Every pointer git follows in a linked worktree's folder, and the files git
// reads as configuration in the git folder, are held read-only.
func TestFenceMountsHoldsEveryGitPointer(t *testing.T) {
	var ws string
	f, _ := mounted(t, func(p *Policy) {
		ws = p.Workspace
		gitInit(t, ws)
		gitIn(t, ws, "worktree", "add", "-q", "wt", "-b", "w")
		for _, name := range []string{".git/info/attributes", ".git/objects/info/alternates"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, name)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		p.ProtectGit = true
	})
	ev := &events{}
	for _, c := range []string{
		"echo ../../evil > .git/worktrees/wt/commondir",
		"echo /tmp/evil > .git/worktrees/wt/gitdir",
		"echo 'gitdir: /tmp/evil' > wt/.git",
		"echo /tmp/evil > .git/objects/info/alternates",
		"echo '* filter=x' > .git/info/attributes",
		"mv .git/worktrees/wt .git/worktrees/away",
	} {
		if out, err := fenceRun(t, f, ws, c, ev.record(nil), "call-pointer"); err == nil {
			t.Errorf("%q succeeded:\n%s", c, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".git", "worktrees", "wt", "commondir")); strings.TrimSpace(string(b)) != "../.." {
		t.Errorf("commondir changed: %q", b)
	}
	if out, err := fenceRun(t, f, filepath.Join(ws, "wt"), "git status --short && echo ok", ev.record(nil), "call-status"); err != nil || !strings.Contains(out, "ok") {
		t.Errorf("git status in the worktree: %v\n%s", err, out)
	}
}

// A commondir a command plants where git never writes one is taken out of
// the workspace before the next command, which is refused and recorded.
func TestFenceMountsQuarantinesAPlantedCommondir(t *testing.T) {
	var ws string
	f, _ := mounted(t, func(p *Policy) {
		ws = p.Workspace
		gitInit(t, ws)
		p.ProtectGit = true
	})
	ev := &events{}
	if out, err := fenceRun(t, f, ws, "echo ../evil > .git/commondir", ev.record(nil), "call-plant"); err != nil {
		t.Fatalf("planting: %v\n%s", err, out)
	}
	out, err := fenceRun(t, f, ws, "echo ran", ev.record(nil), "call-next")
	if err == nil || strings.Contains(out, "ran") {
		t.Fatalf("the command after the plant ran: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".git", "commondir")); err == nil {
		t.Error("the planted commondir is still in the git folder")
	}
	if got := ev.of(EvGitPlanted); len(got) != 1 || got[0]["call_id"] != "call-next" {
		t.Errorf("%s: %v", EvGitPlanted, got)
	}
}

// A linked worktree folder, which a command could point at a git folder of
// its own, refuses the command, as on the process tier.
func TestFenceMountsRefusesALinkedWorktree(t *testing.T) {
	var ws string
	f, _ := mounted(t, func(p *Policy) {
		ws = p.Workspace
		gitInit(t, ws)
		gitIn(t, ws, "worktree", "add", "-q", "wt", "-b", "w")
		wts := filepath.Join(ws, ".git", "worktrees")
		if err := os.Rename(filepath.Join(wts, "wt"), filepath.Join(ws, "hid")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../hid", filepath.Join(wts, "wt")); err != nil {
			t.Fatal(err)
		}
		p.ProtectGit = true
	})
	ev := &events{}
	out, err := fenceRun(t, f, ws, "echo ran", ev.record(nil), "call-linked")
	if err == nil || !strings.Contains(err.Error()+out, "symbolic link") {
		t.Fatalf("a linked worktree folder did not refuse the command: %v\n%s", err, out)
	}
	if got := ev.of(EvGitLinked); len(got) != 1 {
		t.Errorf("%s: %v", EvGitLinked, got)
	}
}
