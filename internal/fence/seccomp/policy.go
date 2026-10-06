package seccomp

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// Profile names the rule set Command builds; it changes whenever the rules do.
const Profile = "command/3"

// ErrUnsupported is returned by Install where the filter cannot be applied.
var ErrUnsupported = errors.New("seccomp: the fence's system-call filter needs Linux on amd64 or arm64")

// Policy is the filter for one command.
type Policy struct {
	// AllowNetwork lets the command create inet, inet6 and route netlink
	// sockets, reaching the host's network unfiltered. Off, socket() is
	// refused for every family. Unix sockets are refused either way, since
	// they reach host daemons that no file rule can stop; socketpair() is
	// allowed either way.
	AllowNetwork bool
	// Abhed is the process id of the Abhed that launches the command, and
	// AbhedGroup its process group. Signals aimed at either are refused:
	// kill, tgkill, tkill and the queued forms naming that process, kill of
	// its group or of every process, and pidfd_send_signal, whose target a
	// filter cannot read.
	Abhed      int
	AbhedGroup int
}

// Command is the default profile for a command the agent runs, launched by
// the Abhed process abhed in process group abhedGroup.
func Command(allowNetwork bool, abhed, abhedGroup int) Policy {
	return Policy{AllowNetwork: allowNetwork, Abhed: abhed, AbhedGroup: abhedGroup}
}

type cond int

const (
	always  cond = iota // the call is refused outright
	maskSet             // refused when arg & k != 0
	below               // refused when arg < k
	equal               // refused when arg == k
	family              // socket: refused unless inet, inet6 or route netlink
	oneOf               // refused when arg equals any of ks
)

type rule struct {
	group string // for Describe
	name  string // system call
	errno uint32
	cond  cond
	arg   uint32
	k     uint32
	ks    []uint32
	note  string // Describe's wording when the rule is narrower than the call
}

// Namespace flags clone(2) accepts: NEWNS, NEWCGROUP, NEWUTS, NEWIPC,
// NEWUSER, NEWPID and NEWNET.
const cloneNamespaceFlags = 0x7e020000

// Values the argument checks compare against.
const (
	afInet          = 2
	afInet6         = 10
	afNetlink       = 16
	netlinkRoute    = 0
	prSetSeccomp    = 22
	seccompGetOpMin = 2 // SECCOMP_GET_ACTION_AVAIL; lower ops install a filter or strict mode
)

func eperm(group string, names ...string) []rule {
	rs := make([]rule, 0, len(names))
	for _, n := range names {
		rs = append(rs, rule{group: group, name: n, errno: errEPERM})
	}
	return rs
}

// rules is the profile, in the order the filter tests them.
func (p Policy) rules() []rule {
	var rs []rule
	rs = append(rs, eperm("process tampering", "ptrace", "process_vm_readv", "process_vm_writev", "kcmp", "pidfd_getfd")...)
	rs = append(rs, eperm("namespaces", "unshare", "setns")...)
	rs = append(rs,
		rule{group: "namespaces", name: "clone", errno: errEPERM, cond: maskSet, arg: 0, k: cloneNamespaceFlags, note: "clone with namespace flags"},
		rule{group: "namespaces", name: "clone3", errno: errENOSYS, note: "clone3 with ENOSYS, so the C library falls back to clone"})
	rs = append(rs, eperm("mounts", "mount", "umount2", "open_tree", "open_tree_attr", "move_mount",
		"fsopen", "fsconfig", "fsmount", "fspick", "mount_setattr", "pivot_root", "chroot")...)
	rs = append(rs, eperm("kernel interfaces", "bpf", "perf_event_open", "userfaultfd",
		"io_uring_setup", "io_uring_enter", "io_uring_register")...)
	rs = append(rs, eperm("modules and kexec", "init_module", "finit_module", "delete_module", "kexec_load", "kexec_file_load")...)
	rs = append(rs, eperm("keyrings", "keyctl", "add_key", "request_key")...)
	rs = append(rs,
		rule{group: "another filter", name: "seccomp", errno: errEPERM, cond: below, arg: 0, k: seccompGetOpMin, note: "seccomp installing a filter or strict mode"},
		rule{group: "another filter", name: "prctl", errno: errEPERM, cond: equal, arg: 0, k: prSetSeccomp, note: "prctl PR_SET_SECCOMP"})
	// Abhed itself, by its process id: kill names a process, or with a
	// negative id a group, and -1 every process the caller may signal.
	pid, kills := uint32(p.Abhed), []uint32{uint32(p.Abhed), uint32(-p.Abhed), 0xffffffff} // #nosec G115 -- pid_t is 32 bits; the kernel reads the low word
	if p.AbhedGroup > 1 && p.AbhedGroup != p.Abhed {
		kills = append(kills, uint32(-p.AbhedGroup)) // #nosec G115 -- as above
	}
	sig := fmt.Sprintf("Abhed's process %d", p.Abhed)
	rs = append(rs,
		rule{group: "signals to Abhed", name: "kill", errno: errEPERM, cond: oneOf, arg: 0, ks: kills,
			note: fmt.Sprintf("kill of %s, its group or every process", sig)},
		rule{group: "signals to Abhed", name: "tkill", errno: errEPERM, cond: oneOf, arg: 0, ks: []uint32{pid}, note: "tkill of it"},
		rule{group: "signals to Abhed", name: "tgkill", errno: errEPERM, cond: oneOf, arg: 0, ks: []uint32{pid}, note: "tgkill of it"},
		rule{group: "signals to Abhed", name: "rt_sigqueueinfo", errno: errEPERM, cond: oneOf, arg: 0, ks: []uint32{pid}, note: "rt_sigqueueinfo of it"},
		rule{group: "signals to Abhed", name: "rt_tgsigqueueinfo", errno: errEPERM, cond: oneOf, arg: 0, ks: []uint32{pid}, note: "rt_tgsigqueueinfo of it"},
		rule{group: "signals to Abhed", name: "pidfd_send_signal", errno: errEPERM, note: "pidfd_send_signal to any process"})
	if p.AllowNetwork {
		rs = append(rs, rule{group: "sockets", name: "socket", errno: errEPERM, cond: family,
			note: "socket families other than inet, inet6 and route netlink, unix included; socketpair allowed"})
	} else {
		rs = append(rs, rule{group: "sockets", name: "socket", errno: errEPERM,
			note: "socket for every family; socketpair allowed"})
	}
	return rs
}

// Program assembles the policy's filter for a Go architecture, amd64 or arm64.
func (p Policy) Program(arch string) (Program, error) {
	if p.Abhed <= 1 {
		return nil, fmt.Errorf("seccomp: the filter needs Abhed's process id, not %d", p.Abhed)
	}
	return build(arch, p.rules())
}

func build(arch string, rules []rule) (Program, error) {
	info, ok := arches[arch]
	if !ok {
		return nil, fmt.Errorf("seccomp: no system-call table for %s", arch)
	}
	a := newAsm()
	// A number means nothing under another architecture's table: kill.
	a.ld(offArch)
	a.jump(opJeq, info.audit, "arch.ok", "")
	a.ret(retKillProcess)
	a.mark("arch.ok")
	a.ld(offNr)
	if info.x32 {
		a.jump(opJge, x32Bit, "", "x32.ok")
		a.ret(retKillProcess)
		a.mark("x32.ok")
	}
	for i, r := range rules {
		nr, ok := info.nr[r.name]
		if !ok {
			return nil, fmt.Errorf("seccomp: no system call %q on %s", r.name, arch)
		}
		next, deny, allow := fmt.Sprintf("%d.next", i), fmt.Sprintf("%d.deny", i), fmt.Sprintf("%d.allow", i)
		a.jump(opJeq, nr, "", next)
		switch r.cond {
		case always:
			a.ret(retErrno | r.errno)
			a.mark(next)
			continue
		case maskSet:
			a.ld(offArg(r.arg))
			a.jump(opJset, r.k, deny, allow)
		case below:
			a.ld(offArg(r.arg))
			a.jump(opJge, r.k, allow, deny)
		case equal:
			a.ld(offArg(r.arg))
			a.jump(opJeq, r.k, deny, allow)
		case oneOf:
			if len(r.ks) == 0 {
				return nil, fmt.Errorf("seccomp: rule %q compares with nothing", r.name)
			}
			a.ld(offArg(r.arg))
			for j, k := range r.ks {
				if j == len(r.ks)-1 {
					a.jump(opJeq, k, deny, allow)
				} else {
					a.jump(opJeq, k, deny, "")
				}
			}
		case family:
			a.ld(offArg(0))
			a.jump(opJeq, afInet, allow, "")
			a.jump(opJeq, afInet6, allow, "")
			a.jump(opJeq, afNetlink, "", deny)
			a.ld(offArg(2))
			a.jump(opJeq, netlinkRoute, allow, deny)
		default:
			return nil, fmt.Errorf("seccomp: rule %q has an unknown condition", r.name)
		}
		a.mark(deny)
		a.ret(retErrno | r.errno)
		a.mark(allow)
		a.ret(retAllow)
		a.mark(next)
	}
	a.ret(retAllow)
	return a.done()
}

// Describe says what the policy applies on this host, for doctor and the
// record: the profile, what it refuses, and the program's size and digest.
func (p Policy) Describe() string {
	arch := runtime.GOARCH
	if _, ok := arches[arch]; runtime.GOOS != "linux" || !ok {
		return fmt.Sprintf("seccomp %s: not available on %s/%s", Profile, runtime.GOOS, arch)
	}
	return p.describe(arch)
}

func (p Policy) describe(arch string) string {
	prog, err := p.Program(arch)
	if err != nil {
		return fmt.Sprintf("seccomp %s on %s: %v", Profile, arch, err)
	}
	network := "off"
	if p.AllowNetwork {
		network = "on"
	}
	var groups []string
	var names []string
	group := ""
	flush := func() {
		if group != "" {
			groups = append(groups, group+" ("+strings.Join(names, ", ")+")")
		}
	}
	for _, r := range p.rules() {
		if r.group != group {
			flush()
			group, names = r.group, nil
		}
		n := r.name
		if r.note != "" {
			n = r.note
		}
		names = append(names, n)
	}
	flush()
	s := fmt.Sprintf("seccomp %s on linux/%s, network %s: kills other architectures", Profile, arch, network)
	if arches[arch].x32 {
		s += " and x32"
	}
	s += "; refuses with EPERM " + strings.Join(groups, "; ")
	return s + fmt.Sprintf("; %d instructions, sha256 %s", len(prog), prog.Digest())
}
