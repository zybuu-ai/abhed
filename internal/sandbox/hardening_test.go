package sandbox

import (
	"context"
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
		{"--ulimit nproc=", "a fork bomb is denial of service against the host"},
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
	}{{"/usr/bin/podman", true}, {"docker", false}} {
		c := &Container{runtime: rt.runtime, policy: Policy{Workspace: "/w"}}
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
