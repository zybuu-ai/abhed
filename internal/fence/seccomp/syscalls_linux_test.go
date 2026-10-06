//go:build linux && (amd64 || arm64)

package seccomp

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// The native table agrees with the system-call numbers Go itself uses.
func TestNativeTableMatchesUnix(t *testing.T) {
	want := map[string]uint32{
		"socket": unix.SYS_SOCKET, "clone": unix.SYS_CLONE, "ptrace": unix.SYS_PTRACE, "pivot_root": unix.SYS_PIVOT_ROOT,
		"prctl": unix.SYS_PRCTL, "chroot": unix.SYS_CHROOT, "mount": unix.SYS_MOUNT, "umount2": unix.SYS_UMOUNT2,
		"init_module": unix.SYS_INIT_MODULE, "delete_module": unix.SYS_DELETE_MODULE, "kexec_load": unix.SYS_KEXEC_LOAD,
		"add_key": unix.SYS_ADD_KEY, "request_key": unix.SYS_REQUEST_KEY, "keyctl": unix.SYS_KEYCTL, "unshare": unix.SYS_UNSHARE,
		"perf_event_open": unix.SYS_PERF_EVENT_OPEN, "setns": unix.SYS_SETNS, "process_vm_readv": unix.SYS_PROCESS_VM_READV,
		"process_vm_writev": unix.SYS_PROCESS_VM_WRITEV, "kcmp": unix.SYS_KCMP, "finit_module": unix.SYS_FINIT_MODULE,
		"seccomp": unix.SYS_SECCOMP, "kexec_file_load": unix.SYS_KEXEC_FILE_LOAD, "bpf": unix.SYS_BPF,
		"userfaultfd": unix.SYS_USERFAULTFD, "io_uring_setup": unix.SYS_IO_URING_SETUP, "io_uring_enter": unix.SYS_IO_URING_ENTER,
		"io_uring_register": unix.SYS_IO_URING_REGISTER, "open_tree": unix.SYS_OPEN_TREE, "move_mount": unix.SYS_MOVE_MOUNT,
		"fsopen": unix.SYS_FSOPEN, "fsconfig": unix.SYS_FSCONFIG, "fsmount": unix.SYS_FSMOUNT, "fspick": unix.SYS_FSPICK,
		"clone3": unix.SYS_CLONE3, "pidfd_getfd": unix.SYS_PIDFD_GETFD, "mount_setattr": unix.SYS_MOUNT_SETATTR,
		"open_tree_attr": unix.SYS_OPEN_TREE_ATTR, "kill": unix.SYS_KILL, "tkill": unix.SYS_TKILL, "tgkill": unix.SYS_TGKILL,
		"rt_sigqueueinfo": unix.SYS_RT_SIGQUEUEINFO, "rt_tgsigqueueinfo": unix.SYS_RT_TGSIGQUEUEINFO,
		"pidfd_send_signal": unix.SYS_PIDFD_SEND_SIGNAL,
	}
	got := arches[runtime.GOARCH].nr
	if len(got) != len(want) {
		t.Errorf("the %s table has %d calls, the check %d", runtime.GOARCH, len(got), len(want))
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s on %s: table %d, unix %d", name, runtime.GOARCH, got[name], n)
		}
	}
	audit := map[string]uint32{"amd64": unix.AUDIT_ARCH_X86_64, "arm64": unix.AUDIT_ARCH_AARCH64}[runtime.GOARCH]
	if arches[runtime.GOARCH].audit != audit {
		t.Errorf("audit arch %#x, want %#x", arches[runtime.GOARCH].audit, audit)
	}
}
