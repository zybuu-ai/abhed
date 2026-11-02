package seccomp

import (
	"strings"
	"testing"
)

// With Notify, every EPERM refusal goes to the listener instead, and nothing
// else changes: the same calls allowed, clone3's ENOSYS kept, other
// architectures still killed.
func TestNotifyRefusesTheSameCallsThroughTheListener(t *testing.T) {
	for _, arch := range archNames {
		info := arches[arch]
		plain, notify := testCommand(false), testCommand(false)
		notify.Notify = true
		pp, np := mustProgram(t, plain, arch), mustProgram(t, notify, arch)
		for _, c := range []call{
			{arch: info.audit, nr: nr(t, arch, "ptrace")},
			{arch: info.audit, nr: nr(t, arch, "unshare")},
			{arch: info.audit, nr: nr(t, arch, "socket"), args: [6]uint64{1}},
			{arch: info.audit, nr: nr(t, arch, "kill"), args: [6]uint64{testAbhed}},
			{arch: info.audit, nr: nr(t, arch, "kill"), args: [6]uint64{4300}},
			{arch: info.audit, nr: nr(t, arch, "clone3")},
			{arch: info.audit, nr: 4000}, // a call the profile does not name
			{arch: info.audit ^ 0x1, nr: 4000},
		} {
			want := run(t, pp, c)
			if want == retErrno|errEPERM {
				want = retUserNotif
			}
			if got := run(t, np, c); got != want {
				t.Errorf("%s nr %d: %s with Notify, want %s", arch, c.nr, retName(got), retName(want))
			}
		}
		if d := notify.describe(arch); !strings.Contains(d, "sent to Abhed") || pp.Digest() == np.Digest() {
			t.Errorf("%s: %s", arch, d)
		}
	}
}

// A refusal names its call, with a socket's family.
func TestRefusalNames(t *testing.T) {
	names := callNames("arm64")
	if names[nr(t, "arm64", "unshare")] != "unshare" {
		t.Fatal("unshare not named")
	}
	if got := refusalName("socket", [6]uint64{1}); got != "socket(unix)" {
		t.Fatalf("socket: %s", got)
	}
	if got := refusalName("socket", [6]uint64{38}); got != "socket(38)" {
		t.Fatalf("socket 38: %s", got)
	}
}
