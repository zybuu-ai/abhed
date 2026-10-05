package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The container flags are the boundary that makes it safe to give a stranger a
// shell. They are easy to weaken by accident — a flag dropped while debugging
// stays dropped — so each one is asserted here with the reason it exists.
//
// This checks the command that would be run, not a live container: the flags
// are the contract, and a test that needs a container runtime would be skipped
// on the machines where it matters most.
func TestContainerFlagsForUntrustedSessions(t *testing.T) {
	c := &Container{policy: Policy{
		Workspace:    "/workspace",
		AllowNetwork: false,
		MaxMemoryMB:  1024,
		MaxProcs:     256,
	}}
	got := strings.Join(c.Command(context.Background(), "/workspace", "echo hi").Args, " ")

	for _, w := range []struct{ flag, why string }{
		{"--cap-drop ALL", "a shell needs no Linux capabilities; CAP_SYS_ADMIN is a container escape"},
		{"--security-opt no-new-privileges", "stops a setuid binary regaining what cap-drop removed"},
		{"--read-only", "a writable rootfs lets a session plant a binary on PATH or persist between commands"},
		{"--network none", "no egress means a stolen secret cannot leave and no payload can be fetched"},
		{"/tmp:rw,noexec", "scratch space that cannot become the place a downloaded binary is executed"},
		{"nosuid", "a setuid binary written to the tmpfs must not confer privilege"},
		{"--pids-limit 256", "a fork bomb is denial of service against the host"},
		{"--memory 1024m --memory-swap 1024m", "the memory bound holds swap too, or it is twice what it says"},
		{"--ulimit fsize=", "an unbounded write fills the host disk"},
		{"--cpus", "one session must not be able to starve every other"},
		{"--ipc private", "no shared memory with any other session"},
		{"--hostname abhed", "the host's name stays hidden, and a host UTS namespace is refused"},
		{"--rm", "the container is destroyed with the command; nothing survives it"},
	} {
		if !strings.Contains(got, w.flag) {
			t.Errorf("missing %q — %s", w.flag, w.why)
		}
	}

	// An nproc limit counts the host's processes of the same uid under
	// rootful Docker, so it stands in only where there is no pids limit.
	if strings.Contains(got, "nproc=") {
		t.Error("an nproc limit beside --pids-limit")
	}
	noPids := &Container{policy: Policy{Workspace: "/workspace"}}
	if !strings.Contains(strings.Join(noPids.Command(context.Background(), "/workspace", "x").Args, " "), "--ulimit nproc=") {
		t.Error("no process bound at all without a pids limit")
	}

	// The workspace is the only writable host path, and the only one mounted.
	if strings.Count(got, " -v ") != 1 {
		t.Errorf("expected exactly one host mount (the workspace), got: %s", got)
	}
	for _, forbidden := range []string{"--privileged", "--cap-add", "-v /:", "/var/run/docker.sock"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("container grants %q, which defeats the sandbox", forbidden)
		}
	}
}

// Podman reads PID and UTS from containers.conf, so they are pinned there;
// Docker refuses "private" for both and never shares them unasked.
func TestPodmanPinsPIDAndUTS(t *testing.T) {
	for _, rt := range []struct {
		runtime string
		want    bool
		podman  bool // what the runtime says it is
	}{{"/usr/bin/podman", true, false}, {"docker", false, false}, {"/usr/local/bin/podman-remote", true, false},
		{"/usr/bin/docker", true, true}} { // the podman-docker wrapper
		c := &Container{runtime: rt.runtime, podman: rt.podman, policy: Policy{Workspace: "/w"}}
		got := strings.Join(c.Command(context.Background(), "/w", "x").Args, " ")
		for _, f := range []string{"--pid private", "--uts private"} {
			if strings.Contains(got, f) != rt.want {
				t.Errorf("%s: %q present = %v, want %v", rt.runtime, f, !rt.want, rt.want)
			}
		}
	}
}

// Network is the difference between a contained mistake and an exfiltration.
func TestNetworkStaysOffUnlessAsked(t *testing.T) {
	off := &Container{policy: Policy{Workspace: "/w", AllowNetwork: false}}
	if !strings.Contains(strings.Join(off.Command(context.Background(), "/w", "x").Args, " "), "--network none") {
		t.Error("AllowNetwork=false did not produce --network none")
	}
	on := &Container{policy: Policy{Workspace: "/w", AllowNetwork: true}}
	if strings.Contains(strings.Join(on.Command(context.Background(), "/w", "x").Args, " "), "--network none") {
		t.Error("AllowNetwork=true still disabled the network — the flag does nothing")
	}
}

// The workspace's .abhed and the state paths a mount would show are hidden,
// as the process tier hides them: a folder by an empty tmpfs, a file by /dev/null.
func TestContainerHidesAbhedState(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ro := t.TempDir()
	users := filepath.Join(ws, "accounts", "users.json")
	stateDirIn := filepath.Join(ro, "records")
	outside := filepath.Join(t.TempDir(), "secrets.json")
	for _, d := range []string{filepath.Dir(users), stateDirIn} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{users, outside} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := &Container{policy: Policy{Workspace: ws, ReadOnlyPaths: []string{ro}, StatePaths: []string{users, stateDirIn, outside}}}
	got := strings.Join(c.Command(context.Background(), ws, "true").Args, " ")
	for _, want := range []string{
		"--tmpfs " + filepath.Join(ws, ".abhed") + ":rw,noexec",
		"-v /dev/null:" + users + ":ro",
		"--tmpfs " + stateDirIn + ":rw,noexec",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, outside) {
		t.Errorf("a state path no mount shows was mounted: %s", got)
	}
	if info, err := os.Stat(filepath.Join(ws, ".abhed")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the .abhed mount point was not made as the person: %v", err)
	}
	// Where the disk folds case, every spelling is covered.
	if _, err := os.Stat(filepath.Join(ws, ".ABHED")); err == nil {
		for _, name := range []string{".ABHED", ".Abhed", ".abhED"} {
			if !strings.Contains(got, "--tmpfs "+filepath.Join(ws, name)+":") {
				t.Errorf("%s not covered", name)
			}
		}
	}
}

// With ProtectGit, the container binds each git folder's config and hooks
// read-only, as bubblewrap does.
func TestContainerProtectsGitFolders(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".git", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := &Container{policy: Policy{Workspace: ws, ProtectGit: true}}
	got := strings.Join(c.Command(context.Background(), ws, "true").Args, " ")
	if hooks := filepath.Join(ws, ".git", "hooks"); !strings.Contains(got, "-v "+hooks+":"+hooks+":ro") {
		t.Fatalf("hooks not bound read-only:\n%s", got)
	}
}

// A .git without hooks or config gets empty ones bound read-only, so a
// command cannot make a hook that runs at the person's next git command.
func TestContainerBindsMissingGitHooksReadOnly(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := &Container{policy: Policy{Workspace: ws, ProtectGit: true}}
	got := strings.Join(c.Command(context.Background(), ws, "true").Args, " ")
	for _, f := range []string{"hooks", "config"} {
		p := filepath.Join(ws, ".git", f)
		if !strings.Contains(got, "-v "+p+":"+p+":ro") {
			t.Errorf("missing %s not bound read-only:\n%s", f, got)
		}
	}
	if info, err := os.Stat(filepath.Join(ws, ".git", "hooks")); err != nil || !info.IsDir() {
		t.Errorf("hooks is not an empty folder: %v", err)
	}
}
