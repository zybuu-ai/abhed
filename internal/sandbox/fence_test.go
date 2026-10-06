package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func fencePolicy(t *testing.T) Policy {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	p := DefaultPolicy(workspace(t))
	p.Tier = TierFence
	return p
}

// The fence counts as the process tier for min_tier, never stronger.
func TestFenceCountsAsTheProcessTier(t *testing.T) {
	if TierFence.Strength() != TierProcess.Strength() {
		t.Fatalf("fence strength %d, process %d", TierFence.Strength(), TierProcess.Strength())
	}
	p := fencePolicy(t)
	p.MinTier = TierContainer
	_, err := Select(p)
	if err == nil || !strings.Contains(err.Error(), "weaker than the required minimum") {
		t.Fatalf("fence under min_tier container: %v", err)
	}
}

// Only the fence can be chosen; an unknown tier is refused, not walked past.
func TestSelectRefusesAnUnknownChosenTier(t *testing.T) {
	p := fencePolicy(t)
	p.Tier = "outer"
	if sb, err := Select(p); err == nil {
		t.Fatalf("tier outer gave %s", sb.Tier())
	}
}

// A surface that keeps paths in the workspace read-only refuses the fence
// without a mount namespace, naming why; no other tier is chosen in its place.
func TestSelectRefusesTheFenceForProtectedWorkspacesWithoutMounts(t *testing.T) {
	for name, set := range map[string]func(*Policy){
		"protect git":     func(p *Policy) { p.ProtectGit = true },
		"write protected": func(p *Policy) { p.WriteProtected = []string{filepath.Join(p.Workspace, ".vscode")} },
	} {
		p := fencePolicy(t)
		p.fenceNoMounts = true
		set(&p)
		sb, err := Select(p)
		if err == nil {
			t.Fatalf("%s: selected %s", name, sb.Tier())
		}
		want := "read-only area"
		if runtime.GOOS != "linux" {
			want = "needs Linux"
		}
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "no other tier") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Abhed's state under a grant refuses the fence: Landlock cannot carve it out.
func TestSelectRefusesTheFenceWhenStateIsGranted(t *testing.T) {
	p := fencePolicy(t)
	home, _ := os.UserHomeDir()
	p.ReadOnlyPaths = []string{home}
	if _, err := Select(p); err == nil || !strings.Contains(err.Error(), ".abhed") {
		t.Fatalf("home granted read-only: %v", err)
	}
	// Without a mount namespace, which would cover it.
	p = fencePolicy(t)
	p.fenceNoMounts = true
	if err := os.Mkdir(filepath.Join(p.Workspace, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(p); err == nil || !strings.Contains(err.Error(), ".abhed") {
		t.Fatalf("workspace .abhed: %v", err)
	}
	// In any case: a filesystem that folds case reads it as .abhed.
	p = fencePolicy(t)
	p.fenceNoMounts = true
	if err := os.Mkdir(filepath.Join(p.Workspace, ".ABhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(p); err == nil || !strings.Contains(err.Error(), ".ABhed") {
		t.Fatalf("workspace .ABhed: %v", err)
	}
}

// A .abhed found in the workspace, in any case, is moved out to home's
// quarantine, reported, and the session's fence runs nothing more.
func TestFenceQuarantinesPlantedState(t *testing.T) {
	p := fencePolicy(t)
	f := NewFence(p)
	f.id = "1-test"
	if err := f.checkPlanted(nil, "", "test"); err != nil || f.planted.Load() != nil {
		t.Fatalf("a clean workspace: %v", err)
	}
	for _, name := range []string{".abhed", ".ABHED"} {
		dir := filepath.Join(p.Workspace, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(`{"users":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Two entries where case matters, one where the filesystem folds it.
	found, err := stateEntries(p.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	planted := len(found)
	var got []map[string]any
	err = f.checkPlanted(func(ev string, pay map[string]any) error {
		if ev != EvFenceStatePlanted {
			t.Errorf("event %s", ev)
		}
		got = append(got, pay)
		return nil
	}, "call-1", "after_command")
	if err == nil || f.planted.Load() == nil {
		t.Fatalf("planted state not refused: %v", err)
	}
	if left, _ := stateEntries(p.Workspace); len(left) != 0 {
		t.Fatalf("still in the workspace: %v", left)
	}
	if len(got) != 1 || got[0]["call_id"] != "call-1" || got[0]["found"] != "after_command" || got[0]["still_present"] != false {
		t.Fatalf("events %v", got)
	}
	entries, _ := got[0]["entries"].([]map[string]any)
	if planted == 0 || len(entries) != planted {
		t.Fatalf("entries %v", got[0]["entries"])
	}
	home, _ := os.UserHomeDir()
	for _, e := range entries {
		dest, _ := e["moved_to"].(string)
		if e["outcome"] != plantMoved || !strings.HasPrefix(dest, filepath.Join(home, ".abhed", "quarantine")+string(filepath.Separator)) {
			t.Errorf("moved to %q: %v", dest, e)
		}
		if _, err := os.Stat(filepath.Join(dest, "users.json")); err != nil {
			t.Errorf("quarantined copy: %v", err)
		}
	}
	if err := f.Command(context.Background(), p.Workspace, "true").Run(); err == nil {
		t.Fatal("a refused session ran a command")
	}
}

// permsMatter skips where a folder's mode does not bind its owner.
func permsMatter(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("folder modes do not bind this user here")
	}
}

// A workspace a command made unlistable counts as holding planted state: a
// .abhed in it is still found by name, and the session is refused, recorded
// and reported however much of it could be taken out.
func TestFenceUnlistableWorkspaceCountsAsPlanted(t *testing.T) {
	permsMatter(t)
	for _, tc := range []struct {
		mode    os.FileMode
		plant   bool
		outcome string
	}{
		{0o100, false, ""},
		{0o100, true, plantRemaining},
		{0o300, true, plantMoved},
		// Listed, but no entry in it can be looked up.
		{0o600, false, ""},
		{0o600, true, plantRemaining},
		{0o400, true, plantRemaining},
	} {
		p := fencePolicy(t)
		f := NewFence(p)
		f.id = "1-test"
		if tc.plant {
			if err := os.MkdirAll(filepath.Join(p.Workspace, ".abhed"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(p.Workspace, tc.mode); err != nil {
			t.Fatal(err)
		}
		var got []map[string]any
		err := f.checkPlanted(func(_ string, pay map[string]any) error { got = append(got, pay); return nil }, "", "after_command")
		_ = os.Chmod(p.Workspace, 0o700)
		if err == nil || f.planted.Load() == nil || !strings.Contains(err.Error(), "cannot be listed") {
			t.Fatalf("mode %o: not refused: %v", tc.mode, err)
		}
		if len(got) != 1 || got[0]["listing_error"] == nil || got[0]["still_present"] != true {
			t.Fatalf("mode %o: event %v", tc.mode, got)
		}
		entries, _ := got[0]["entries"].([]map[string]any)
		if !tc.plant {
			if len(entries) != 0 {
				t.Fatalf("mode %o: entries %v", tc.mode, entries)
			}
			continue
		}
		if len(entries) != 1 || entries[0]["outcome"] != tc.outcome {
			t.Fatalf("mode %o: entries %v", tc.mode, entries)
		}
		left, _ := stateEntries(p.Workspace)
		if (tc.outcome == plantRemaining) != (len(left) == 1) {
			t.Fatalf("mode %o: left %v", tc.mode, left)
		}
		if tc.outcome == plantRemaining && !strings.Contains(err.Error(), "still in the workspace") {
			t.Fatalf("mode %o: %v", tc.mode, err)
		}
	}
}

// A .abhed a command locked, with no permission but search on it, is still
// taken out whole, and its owner can read and remove it afterwards.
func TestFenceTakesOutLockedState(t *testing.T) {
	permsMatter(t)
	p := fencePolicy(t)
	f := NewFence(p)
	f.id = "1-test"
	dir := filepath.Join(p.Workspace, ".abhed")
	if err := os.MkdirAll(filepath.Join(dir, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(`{"users":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(dir, "deep"), dir} {
		if err := os.Chmod(d, 0o100); err != nil {
			t.Fatal(err)
		}
	}
	var got []map[string]any
	if err := f.checkPlanted(func(_ string, pay map[string]any) error { got = append(got, pay); return nil }, "", "after_command"); err == nil {
		t.Fatal("not reported")
	}
	entries, _ := got[0]["entries"].([]map[string]any)
	if len(entries) != 1 || entries[0]["outcome"] != plantMoved {
		t.Fatalf("entries %v", entries)
	}
	dest := entries[0]["moved_to"].(string)
	if _, err := os.ReadFile(filepath.Join(dest, "users.json")); err != nil {
		t.Fatalf("quarantined copy: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(dest)); err != nil {
		t.Fatalf("the quarantined copy cannot be removed: %v", err)
	}
}

// Release frees only a pending command that never started, and only once.
func TestReleaseOnlyUnstarted(t *testing.T) {
	Release(nil)
	calls := 0
	unstarted := &exec.Cmd{}
	pendingLaunches.Store(unstarted, func() { calls++ })
	Release(unstarted)
	Release(unstarted)
	if calls != 1 {
		t.Fatalf("released %d times", calls)
	}
	started := &exec.Cmd{Process: &os.Process{Pid: 1}}
	pendingLaunches.Store(started, func() { calls++ })
	defer pendingLaunches.Delete(started)
	Release(started)
	if calls != 1 {
		t.Fatal("a started command was released")
	}
}

// A fence that did not qualify builds commands that never start.
func TestUnqualifiedFenceRunsNothing(t *testing.T) {
	p := fencePolicy(t)
	p.ProtectGit, p.fenceNoMounts = true, true
	f := NewFence(p)
	marker := filepath.Join(p.Workspace, "ran")
	err := f.Command(context.Background(), p.Workspace, "touch "+marker).Run()
	if err == nil || !strings.Contains(err.Error(), "not run") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("the command ran")
	}
}

// With tier unset, Select walks the tiers as before and never builds the fence.
func TestSelectUnchangedWithoutATier(t *testing.T) {
	p := DefaultPolicy(workspace(t))
	p.MinTier = TierNone
	sb, err := Select(p)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Tier() == TierFence {
		t.Fatal("the fence was chosen without being asked for")
	}
}

// Describe says plainly what the fence covers and what it does not.
func TestFenceDescribe(t *testing.T) {
	p := fencePolicy(t)
	p.CPUPercent = 150
	off := NewFence(p).Describe()
	for _, want := range []string{"preview", "Landlock", "seccomp command/3", "every socket refused", "memory 4096 MB",
		"at most 512 processes", "CPU 150%", "not covered", "Abhed itself", "network filtering", "signal your other processes",
		"no other terminal", "is renamed out of the way once the command ends and moved to ~/.abhed/quarantine where it can be",
		"when the workspace can no longer be listed", "the session's commands still running are ended", "signals to Abhed's process id",
		"a hard link to state made before the session", "while it runs and until the check that follows it (Abhed can read it then)",
		"after an unclean exit (nothing is moved)", "a .abhed in a subfolder or an added folder (not checked)"} {
		if !strings.Contains(off, want) {
			t.Errorf("Describe lacks %q:\n%s", want, off)
		}
	}
	p.AllowNetwork = true
	if on := NewFence(p).Describe(); !strings.Contains(on, "the host's network, unfiltered") || !strings.Contains(on, "unix sockets refused") {
		t.Errorf("network on:\n%s", on)
	}
}

// The launcher's spec grants the workspace and private temp, and only checks
// Abhed's state stays outside every grant.
func TestFenceSpec(t *testing.T) {
	p := fencePolicy(t)
	p.AllowNetwork = false
	state := t.TempDir()
	for _, n := range []string{"secrets.json", "secrets.d"} {
		if err := os.WriteFile(filepath.Join(state, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p.StatePaths = []string{filepath.Join(state, "secrets.json"), filepath.Join(p.Workspace, ".abhed", "users.json")}
	f := NewFence(p)
	f.tmp = filepath.Join(t.TempDir(), "fence-tmp")
	s := f.spec()
	if !slices.Contains(s.Write, p.Workspace) || !slices.Contains(s.Write, f.tmp) || len(s.Write) != 2 {
		t.Errorf("write grants %v", s.Write)
	}
	home, _ := os.UserHomeDir()
	for _, d := range []string{filepath.Join(home, ".abhed"), filepath.Join(RealPath(state), "secrets.json"), filepath.Join(RealPath(state), "secrets.d")} {
		if !slices.Contains(s.Deny, d) {
			t.Errorf("deny lacks %s: %v", d, s.Deny)
		}
	}
	// A state file not yet made holds nothing, and is no reason to refuse.
	if slices.Contains(s.Deny, filepath.Join(p.Workspace, ".abhed", "users.json")) {
		t.Errorf("deny names a missing state file: %v", s.Deny)
	}
	for _, d := range s.Devices {
		if strings.HasPrefix(d, "/dev/pts") || d == "/dev/ptmx" {
			t.Errorf("a pty is granted: %v", s.Devices)
		}
	}
	if !s.DenyTCP {
		t.Error("network off, yet TCP not denied")
	}
	if err := s.Validate(); err != nil {
		t.Errorf("the spec does not validate: %v", err)
	}
	env := strings.Join(f.env(), "\n")
	for _, want := range []string{"ABHED_SANDBOX=fence", "TMPDIR=" + f.tmp, "HOME=" + filepath.Join(f.tmp, "home")} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s:\n%s", want, env)
		}
	}
}

// A leaf is named by the call id, made safe and unique.
func TestFenceLeafName(t *testing.T) {
	f := NewFence(Policy{})
	a, b := f.leafName("toolu_01/../x y"), f.leafName("toolu_01/../x y")
	if a == b || strings.ContainsAny(a, "/ ") || !strings.HasPrefix(a, "toolu_01_") {
		t.Fatalf("names %q %q", a, b)
	}
	if n := f.leafName(""); !strings.HasPrefix(n, "cmd-") {
		t.Fatalf("no call id: %q", n)
	}
	if n := f.leafName(strings.Repeat("a", 200)); len(n) > 64 {
		t.Fatalf("long id: %d bytes", len(n))
	}
}

// The launch a caller passes reaches the backend.
func TestLaunchCarriesTheCall(t *testing.T) {
	ctx := WithLaunch(context.Background(), Launch{CallID: "c1"})
	if got := LaunchOf(ctx).CallID; got != "c1" {
		t.Fatalf("call id %q", got)
	}
	if LaunchOf(context.Background()).Record != nil {
		t.Fatal("a launch from nowhere")
	}
}

// A closed fence runs nothing more, and Close of one never qualified makes
// nothing.
func TestClosedFenceRunsNothing(t *testing.T) {
	p := fencePolicy(t)
	f := NewFence(p)
	if err := Close(f); err != nil {
		t.Fatal(err)
	}
	if f.host != nil || f.tmp != "" {
		t.Fatal("Close qualified the fence")
	}
	if err := f.Command(context.Background(), p.Workspace, "true").Run(); err == nil {
		t.Fatal("a closed fence ran a command")
	}
}

// In the mount mode, Describe says what the namespace holds, and no longer
// names the planted-state window it closes.
func TestFenceDescribeMounts(t *testing.T) {
	p := fencePolicy(t)
	p.ProtectGit = true
	f := NewFence(p)
	f.mounts = true
	d := f.Describe()
	for _, want := range []string{"mode mount_namespace", "empty tmpfs", "bound read-only", "~/.abhed/skills", "a .abhed in a subfolder or an added folder (not covered)"} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe lacks %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "until the check that follows it") {
		t.Errorf("Describe names a window the mounts close:\n%s", d)
	}
	if f.Mode() != FenceModeMounts || NewFence(p).Mode() != FenceModeLandlock {
		t.Errorf("modes %s %s", f.Mode(), NewFence(p).Mode())
	}
}

// The plan holds git's config and hooks and the write-protected paths
// read-only, pins the folders holding them, covers the workspace's .abhed
// and hides state kept elsewhere in the workspace; a link is refused.
func TestFencePlan(t *testing.T) {
	p := fencePolicy(t)
	ws := p.Workspace
	for _, d := range []string{".git/hooks", "sub/.git", ".vscode", ".abhed", "state.d"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".git/config", ".vscode/settings.json", "users.json", "state.d/x"} {
		if err := os.WriteFile(filepath.Join(ws, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p.ProtectGit = true
	p.WriteProtected = []string{filepath.Join(ws, ".vscode", "settings.json"), filepath.Join(ws, ".vscode", "missing.json"), "/etc/outside"}
	p.StatePaths = []string{filepath.Join(ws, "state.d"), filepath.Join(ws, ".abhed", "inner.json")}
	f := NewFence(p)
	f.mounts = true
	plan, err := f.plan()
	if err != nil {
		t.Fatal(err)
	}
	// A state file in the workspace outside .abhed, which sandboxconfig
	// refuses, refuses the command rather than being left open.
	withFile := NewFence(p)
	withFile.mounts = true
	withFile.policy.StatePaths = append(slices.Clone(p.StatePaths), filepath.Join(ws, "users.json"))
	if _, err := withFile.plan(); err == nil || !strings.Contains(err.Error(), "state file inside the workspace") {
		t.Errorf("a state file in the workspace: %v", err)
	}
	for _, want := range []string{".git/hooks", ".git/config", "sub/.git/hooks", "sub/.git/config", ".vscode/settings.json"} {
		if !slices.Contains(plan.ReadOnly, filepath.FromSlash(want)) {
			t.Errorf("read-only lacks %s: %v", want, plan.ReadOnly)
		}
	}
	for _, want := range []string{".git", "sub/.git", ".vscode"} {
		if !slices.Contains(plan.Pin, filepath.FromSlash(want)) {
			t.Errorf("pins lack %s: %v", want, plan.Pin)
		}
	}
	if !slices.Equal(plan.Empty, []string{".abhed", "state.d"}) {
		t.Errorf("hidden: %v", plan.Empty)
	}
	if err := os.Symlink("/tmp", filepath.Join(ws, ".vscode", "link.json")); err != nil {
		t.Fatal(err)
	}
	f.policy.WriteProtected = append(f.policy.WriteProtected, filepath.Join(ws, ".vscode", "link.json"))
	if _, err := f.plan(); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("a linked protected path: %v", err)
	}
}

// ~/.abhed/skills is granted read and run, inside the denied state, and the
// rest of ~/.abhed stays denied; in the mount mode the covered .abhed and
// the state the mounts hide are not denied, since no grant can reach them.
func TestFenceSpecGrantsSkills(t *testing.T) {
	p := fencePolicy(t)
	home, _ := os.UserHomeDir()
	skills := filepath.Join(home, ".abhed", "skills")
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p.Workspace, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	p.StatePaths = []string{filepath.Join(p.Workspace, "users.json")}
	if err := os.WriteFile(p.StatePaths[0], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := NewFence(p)
	s := f.spec()
	if !slices.Contains(s.Exec, skills) || !slices.Contains(s.Within, skills) || slices.Contains(s.Write, skills) {
		t.Errorf("skills: exec %v within %v", s.Exec, s.Within)
	}
	if !slices.Contains(s.Deny, filepath.Join(home, ".abhed")) {
		t.Errorf("deny lacks ~/.abhed: %v", s.Deny)
	}
	if err := s.Validate(); err == nil {
		t.Error("landlock only: the workspace's .abhed under the write grant validated")
	}
	f.mounts = true
	if err := f.prepareStateMount(); err != nil {
		t.Fatal(err)
	}
	s = f.spec()
	if err := s.Validate(); err != nil {
		t.Errorf("mounts: %v (deny %v)", err, s.Deny)
	}
}

// In the mount mode the fence makes the workspace's .abhed to mount over
// when it is missing, does not take it for planted state, and leaves it at
// Close, for another fence on the workspace mounts over it too; that one
// takes it for its own as well. Another spelling is still planted.
func TestFenceStateMountIsNotPlanted(t *testing.T) {
	p := fencePolicy(t)
	f := NewFence(p)
	f.mounts = true
	if err := f.prepareStateMount(); err != nil || !f.madeState || f.stateHeld {
		t.Fatalf("prepare: %v made %v held %v", err, f.madeState, f.stateHeld)
	}
	other := NewFence(p)
	other.mounts = true
	if err := other.prepareStateMount(); err != nil || other.madeState || other.stateHeld {
		t.Fatalf("the second fence: %v made %v held %v", err, other.madeState, other.stateHeld)
	}
	if err := f.checkPlanted(nil, "", "before_command"); err != nil {
		t.Fatalf("the covered .abhed was taken for planted: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(p.Workspace, ".abhed")); err != nil || !info.IsDir() {
		t.Fatalf("the made .abhed did not outlive Close: %v", err)
	}
	if err := other.checkPlanted(nil, "", "before_command"); err != nil {
		t.Fatalf("the other fence took the .abhed for planted: %v", err)
	}
	if runtime.GOOS == "darwin" {
		return // APFS folds case: .ABHED is the same entry
	}
	g := NewFence(p)
	g.mounts = true
	if err := g.prepareStateMount(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(p.Workspace, ".ABHED"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := g.checkPlanted(nil, "", "before_command"); err == nil {
		t.Error("another spelling of .abhed was not taken out")
	}
}

// The .abhed the fence covers is its own only while it is the same folder
// and, when it was empty at qualifying, still empty: a file found in it is
// planted, and so is a folder made again in its place. One that already
// held state is Abhed's, whatever it holds now.
func TestFenceStateMountRule(t *testing.T) {
	p := fencePolicy(t)
	state := filepath.Join(p.Workspace, ".abhed")
	f := NewFence(p)
	f.mounts = true
	if err := f.prepareStateMount(); err != nil {
		t.Fatal(err)
	}
	if !f.isStateMount(state) {
		t.Fatal("the made .abhed is not the fence's own")
	}
	if err := os.WriteFile(filepath.Join(state, "users.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f.isStateMount(state) {
		t.Error("a file in the made .abhed was not taken for planted")
	}
	before, _ := folderIdentity(state)
	if err := f.checkPlanted(nil, "", "after_command"); err == nil {
		t.Error("checkPlanted passed a file in the made .abhed")
	}
	// Its contents are taken out and the folder stays: other fences on the
	// workspace mount over it.
	if left, err := os.ReadDir(state); err != nil || len(left) != 0 {
		t.Errorf("the covered .abhed after the check: %v %v", err, left)
	}
	if now, err := folderIdentity(state); err != nil || !now.same(before) {
		t.Errorf("the covered .abhed was moved or made again: %v", err)
	}
	home, _ := os.UserHomeDir()
	if moved, _ := filepath.Glob(filepath.Join(home, ".abhed", QuarantineDir, "*", "users.json")); len(moved) != 1 {
		t.Errorf("users.json in quarantine: %v", moved)
	}

	// A folder made again in its place is not taken for it, even empty:
	// the birth time, or failing that the inode, tells them apart.
	g := NewFence(p)
	g.mounts = true
	if err := g.prepareStateMount(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if id, err := folderIdentity(state); err == nil && id.same(*g.stateID) {
		t.Skip("the filesystem reused the inode and records no birth time")
	}
	if g.isStateMount(state) {
		t.Error("a .abhed made again was taken for the fence's own")
	}

	// A .abhed that held state when the fence qualified is Abhed's own.
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewFence(p)
	h.mounts = true
	if err := h.prepareStateMount(); err != nil || !h.stateHeld || h.madeState {
		t.Fatalf("prepare: %v held %v made %v", err, h.stateHeld, h.madeState)
	}
	if err := os.WriteFile(filepath.Join(state, "users.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.isStateMount(state) || h.isStateMount(filepath.Join(p.Workspace, "other")) {
		t.Error("the .abhed that held state is not the fence's own")
	}
}

// Two fences on one workspace: content that appears in the covered .abhed
// after the first qualified with it empty is taken out by the first, and
// the folder stays for the second, which held state then.
func TestFenceStateMountKeepsTheSharedFolder(t *testing.T) {
	p := fencePolicy(t)
	state := filepath.Join(p.Workspace, ".abhed")
	a := NewFence(p)
	a.mounts = true
	if err := a.prepareStateMount(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "users.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := NewFence(p)
	b.mounts = true
	if err := b.prepareStateMount(); err != nil || !b.stateHeld {
		t.Fatalf("the second fence: %v held %v", err, b.stateHeld)
	}
	var got []map[string]any
	rec := func(ev string, pay map[string]any) error { got = append(got, pay); return nil }
	if err := a.checkPlanted(rec, "", "before_command"); err == nil || !strings.Contains(err.Error(), "was empty when this session's fence started") {
		t.Fatalf("the first fence's check: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events: %v", got)
	}
	entries := got[0]["entries"].([]map[string]any)
	if len(entries) != 1 || entries[0]["outcome"] != plantEmptied || len(entries[0]["contents"].([]map[string]any)) != 1 {
		t.Errorf("entries: %v", entries)
	}
	if !b.isStateMount(state) {
		t.Error("the second fence lost its .abhed")
	}
	if err := b.checkPlanted(nil, "", "before_command"); err != nil {
		t.Errorf("the second fence's check: %v", err)
	}
}

// A .abhed that cannot be listed when the fence qualifies is not known to
// be empty, and is not taken for state the mounts cover: the fence refuses.
func TestFenceStateMountRefusesAnUnlistableFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists any folder")
	}
	p := fencePolicy(t)
	state := filepath.Join(p.Workspace, ".abhed")
	if err := os.Mkdir(state, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) })
	f := NewFence(p)
	f.mounts = true
	if err := f.prepareStateMount(); err == nil || !strings.Contains(err.Error(), "cannot be listed") {
		t.Fatalf("an unlistable .abhed: %v (held %v)", err, f.stateHeld)
	}
}

// skills.dirs are read and run beside ~/.abhed/skills; one that holds or
// sits inside Abhed's state, or inside the workspace, is left out, and the
// spec still validates.
func TestFenceSpecGrantsConfiguredSkills(t *testing.T) {
	p := fencePolicy(t)
	home, _ := os.UserHomeDir()
	team := filepath.Join(t.TempDir(), "team-skills")
	for _, d := range []string{team, filepath.Join(home, ".abhed", "skills", "sub"), filepath.Join(home, ".abhed", "records"), filepath.Join(p.Workspace, "skills")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p.SkillDirs = []string{team, filepath.Join(home, ".abhed", "skills", "sub"), filepath.Join(home, ".abhed", "records"),
		home, filepath.Join(p.Workspace, "skills"), filepath.Join(t.TempDir(), "missing")}
	f := NewFence(p)
	s := f.spec()
	if !slices.Contains(s.Exec, RealPath(team)) || slices.Contains(s.Within, RealPath(team)) || slices.Contains(s.Write, RealPath(team)) {
		t.Errorf("the configured folder: exec %v within %v", s.Exec, s.Within)
	}
	for _, out := range []string{filepath.Join(home, ".abhed", "records"), home, filepath.Join(p.Workspace, "skills")} {
		if slices.ContainsFunc(s.Exec, func(e string) bool { return e == out || e == RealPath(out) }) {
			t.Errorf("%s granted: %v", out, s.Exec)
		}
	}
	if err := s.Validate(); err != nil {
		t.Errorf("the spec does not validate: %v", err)
	}
	_, left := f.configuredSkills(s.Deny)
	if len(left) != 4 {
		t.Errorf("left out %v", left)
	}
	q := f.Qualification()
	if !slices.Contains(q["skills"].([]string), RealPath(team)) || len(q["skills_left_out"].([]string)) != 4 {
		t.Errorf("qualification: skills %v left out %v", q["skills"], q["skills_left_out"])
	}
}
