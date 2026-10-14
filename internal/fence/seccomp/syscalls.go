package seccomp

// archInfo is what the filter needs to know about one architecture.
type archInfo struct {
	audit uint32 // AUDIT_ARCH_* value the kernel reports in seccomp_data.arch
	x32   bool   // the x32 ABI shares audit with this one, flagged by nr bit 30
	// cloneFlags is the argument clone(2) takes its flags in: the second on
	// s390x, whose clone takes the stack first.
	cloneFlags uint32
	// socketcall: socket(2) is also reachable through the socketcall(2)
	// multiplexer, which glibc and Go use on s390x.
	socketcall bool
	// umount: the old umount(2) exists beside umount2.
	umount bool
	nr     map[string]uint32
}

// x32Bit marks an x32 system call on amd64.
const x32Bit = 0x40000000

// auditArchLE is the kernel's __AUDIT_ARCH_LE: set in an audit value whose
// architecture stores seccomp_data little-endian.
const auditArchLE = 0x40000000

// AuditArch is the AUDIT_ARCH_* value of a Go architecture the filter has a
// table for.
func AuditArch(goarch string) (uint32, bool) {
	a, ok := arches[goarch]
	return a.audit, ok
}

// argLow is the offset of argument i's low word in seccomp_data: its first
// word on a little-endian machine, its second on a big-endian one.
func (a archInfo) argLow(i uint32) uint32 {
	off := 16 + 8*i
	if a.audit&auditArchLE == 0 {
		off += 4
	}
	return off
}

// Only the calls the profile names, numbered from the kernel's syscall tables;
// the tests hold them to golang.org/x/sys/unix.
var arches = map[string]archInfo{
	"amd64": {audit: 0xc000003e, x32: true, nr: map[string]uint32{
		"socket":            41,
		"clone":             56,
		"kill":              62,
		"rt_sigqueueinfo":   129,
		"tkill":             200,
		"tgkill":            234,
		"rt_tgsigqueueinfo": 297,
		"pidfd_send_signal": 424,
		"ptrace":            101,
		"pivot_root":        155,
		"prctl":             157,
		"chroot":            161,
		"mount":             165,
		"umount2":           166,
		"init_module":       175,
		"delete_module":     176,
		"kexec_load":        246,
		"add_key":           248,
		"request_key":       249,
		"keyctl":            250,
		"unshare":           272,
		"perf_event_open":   298,
		"setns":             308,
		"process_vm_readv":  310,
		"process_vm_writev": 311,
		"kcmp":              312,
		"finit_module":      313,
		"seccomp":           317,
		"kexec_file_load":   320,
		"bpf":               321,
		"userfaultfd":       323,
		"io_uring_setup":    425,
		"io_uring_enter":    426,
		"io_uring_register": 427,
		"open_tree":         428,
		"move_mount":        429,
		"fsopen":            430,
		"fsconfig":          431,
		"fsmount":           432,
		"fspick":            433,
		"clone3":            435,
		"pidfd_getfd":       438,
		"mount_setattr":     442,
		"open_tree_attr":    467,
	}},
	"arm64": {audit: 0xc00000b7, nr: map[string]uint32{
		"umount2":           39,
		"mount":             40,
		"pivot_root":        41,
		"chroot":            51,
		"unshare":           97,
		"kexec_load":        104,
		"init_module":       105,
		"delete_module":     106,
		"ptrace":            117,
		"kill":              129,
		"tkill":             130,
		"tgkill":            131,
		"rt_sigqueueinfo":   138,
		"rt_tgsigqueueinfo": 240,
		"pidfd_send_signal": 424,
		"prctl":             167,
		"socket":            198,
		"add_key":           217,
		"request_key":       218,
		"keyctl":            219,
		"clone":             220,
		"perf_event_open":   241,
		"setns":             268,
		"process_vm_readv":  270,
		"process_vm_writev": 271,
		"kcmp":              272,
		"finit_module":      273,
		"seccomp":           277,
		"bpf":               280,
		"userfaultfd":       282,
		"kexec_file_load":   294,
		"io_uring_setup":    425,
		"io_uring_enter":    426,
		"io_uring_register": 427,
		"open_tree":         428,
		"move_mount":        429,
		"fsopen":            430,
		"fsconfig":          431,
		"fsmount":           432,
		"fspick":            433,
		"clone3":            435,
		"pidfd_getfd":       438,
		"mount_setattr":     442,
		"open_tree_attr":    467,
	}}, // 64-bit only: a 31-bit task reports AUDIT_ARCH_S390 and is killed.
	"s390x": {audit: 0x80000016, cloneFlags: 1, socketcall: true, umount: true, nr: map[string]uint32{
		"mount":             21,
		"umount":            22,
		"ptrace":            26,
		"kill":              37,
		"umount2":           52,
		"chroot":            61,
		"socketcall":        102,
		"clone":             120,
		"init_module":       128,
		"delete_module":     129,
		"prctl":             172,
		"rt_sigqueueinfo":   178,
		"pivot_root":        217,
		"tkill":             237,
		"tgkill":            241,
		"kexec_load":        277,
		"add_key":           278,
		"request_key":       279,
		"keyctl":            280,
		"unshare":           303,
		"rt_tgsigqueueinfo": 330,
		"perf_event_open":   331,
		"setns":             339,
		"process_vm_readv":  340,
		"process_vm_writev": 341,
		"kcmp":              343,
		"finit_module":      344,
		"seccomp":           348,
		"bpf":               351,
		"userfaultfd":       355,
		"socket":            359,
		"kexec_file_load":   381,
		"pidfd_send_signal": 424,
		"io_uring_setup":    425,
		"io_uring_enter":    426,
		"io_uring_register": 427,
		"open_tree":         428,
		"move_mount":        429,
		"fsopen":            430,
		"fsconfig":          431,
		"fsmount":           432,
		"fspick":            433,
		"clone3":            435,
		"pidfd_getfd":       438,
		"mount_setattr":     442,
		"open_tree_attr":    467,
	}},
}
