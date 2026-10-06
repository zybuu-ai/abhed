package seccomp

import (
	"encoding/binary"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden programs in testdata")

// call is a system call as the filter sees it.
type call struct {
	arch uint32
	nr   uint32
	args [6]uint64
}

// run interprets prog against c as the kernel would, failing the test on
// anything the kernel's checker would refuse.
func run(t *testing.T, prog Program, c call) uint32 {
	t.Helper()
	var data [64]byte
	binary.LittleEndian.PutUint32(data[0:], c.nr)
	binary.LittleEndian.PutUint32(data[4:], c.arch)
	for i, a := range c.args {
		binary.LittleEndian.PutUint64(data[16+8*i:], a)
	}
	var acc uint32
	for pc := 0; pc < len(prog); {
		in := prog[pc]
		next := pc + 1
		switch in.Code {
		case opLdAbs:
			if in.K%4 != 0 || in.K+4 > uint32(len(data)) {
				t.Fatalf("pc %d: load at %d", pc, in.K)
			}
			acc = binary.LittleEndian.Uint32(data[in.K:])
		case opJeq, opJge, opJset:
			var taken bool
			switch in.Code {
			case opJeq:
				taken = acc == in.K
			case opJge:
				taken = acc >= in.K
			default:
				taken = acc&in.K != 0
			}
			if taken {
				next += int(in.Jt)
			} else {
				next += int(in.Jf)
			}
		case opRet:
			return in.K
		default:
			t.Fatalf("pc %d: opcode %#x", pc, in.Code)
		}
		pc = next
	}
	t.Fatal("the program ran off its end")
	return 0
}

func mustProgram(t *testing.T, p Policy, arch string) Program {
	t.Helper()
	prog, err := p.Program(arch)
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

func nr(t *testing.T, arch, name string) uint32 {
	t.Helper()
	n, ok := arches[arch].nr[name]
	if !ok {
		t.Fatalf("no %s on %s", name, arch)
	}
	return n
}

var archNames = []string{"amd64", "arm64"}

// The Abhed process and group the test programs guard; the goldens pin them.
const (
	testAbhed      = 4242
	testAbhedGroup = 4200
)

func testCommand(network bool) Policy { return Command(network, testAbhed, testAbhedGroup) }

// neg is -n as the filter sees a negative pid_t: its low word.
func neg(n int) uint64 { return uint64(uint32(-n)) } // #nosec G115 -- a test pid

// Signals aimed at Abhed are refused, by its process id, its group or kill's
// every process; the same calls aimed elsewhere are allowed.
func TestSignalsToAbhedRefused(t *testing.T) {
	const eperm = retErrno | errEPERM
	type tc struct {
		name string
		args [6]uint64
		want uint32
	}
	cases := []tc{
		{"kill", [6]uint64{testAbhed, 9}, eperm},
		{"kill", [6]uint64{testAbhed, 0}, eperm},
		{"kill", [6]uint64{neg(testAbhed), 9}, eperm},
		{"kill", [6]uint64{neg(testAbhedGroup), 15}, eperm},
		{"kill", [6]uint64{0xffffffff, 9}, eperm},                        // -1, every process
		{"kill", [6]uint64{0xffff_ffff_0000_0000 | testAbhed, 9}, eperm}, // high word ignored, as the kernel does
		{"kill", [6]uint64{testAbhed + 1, 9}, retAllow},
		{"kill", [6]uint64{testAbhedGroup, 9}, retAllow}, // the group's leader by pid is another process
		{"kill", [6]uint64{0, 15}, retAllow},             // the command's own group
		{"kill", [6]uint64{neg(testAbhed + 7), 15}, retAllow},
		{"tkill", [6]uint64{testAbhed, 9}, eperm},
		{"tkill", [6]uint64{testAbhed + 1, 9}, retAllow},
		{"tgkill", [6]uint64{testAbhed, testAbhed, 9}, eperm},
		{"tgkill", [6]uint64{testAbhed + 1, testAbhed + 1, 9}, retAllow},
		{"rt_sigqueueinfo", [6]uint64{testAbhed, 9}, eperm},
		{"rt_sigqueueinfo", [6]uint64{testAbhed + 1, 9}, retAllow},
		{"rt_tgsigqueueinfo", [6]uint64{testAbhed, testAbhed, 9}, eperm},
		{"rt_tgsigqueueinfo", [6]uint64{testAbhed + 1, testAbhed + 1, 9}, retAllow},
		{"pidfd_send_signal", [6]uint64{3, 9}, eperm},
	}
	for _, arch := range archNames {
		for _, network := range []bool{false, true} {
			prog := mustProgram(t, testCommand(network), arch)
			for _, c := range cases {
				if got := run(t, prog, call{arch: arches[arch].audit, nr: nr(t, arch, c.name), args: c.args}); got != c.want {
					t.Errorf("%s network=%v: %s%v gave %s, want %s", arch, network, c.name, c.args[:3], retName(got), retName(c.want))
				}
			}
		}
	}
	// A group that is Abhed's own pid adds nothing; one of 0 or 1 is not a group to guard.
	for _, group := range []int{testAbhed, 0, 1} {
		prog := mustProgram(t, Command(false, testAbhed, group), "amd64")
		if got := run(t, prog, call{arch: arches["amd64"].audit, nr: nr(t, "amd64", "kill"), args: [6]uint64{neg(testAbhed)}}); got != eperm {
			t.Errorf("group %d: kill of Abhed's group gave %s", group, retName(got))
		}
		if got := run(t, prog, call{arch: arches["amd64"].audit, nr: nr(t, "amd64", "kill"), args: [6]uint64{neg(1)}}); group == 1 && got != eperm {
			t.Errorf("group 1: kill(-1) gave %s", retName(got))
		}
	}
}

// Without Abhed's process id there is nothing to guard, and no program.
func TestProgramNeedsAbhed(t *testing.T) {
	for _, pid := range []int{0, 1, -5} {
		if _, err := Command(false, pid, 0).Program("amd64"); err == nil {
			t.Errorf("pid %d built a program", pid)
		}
	}
}

// Every program checks the architecture first, jumps only forward and within
// the program, and ends allowing what no rule names.
func TestProgramShape(t *testing.T) {
	for _, arch := range archNames {
		for _, network := range []bool{false, true} {
			prog := mustProgram(t, testCommand(network), arch)
			if len(prog) < 4 || prog[0] != (Instruction{Code: opLdAbs, K: offArch}) ||
				prog[1].Code != opJeq || prog[1].K != arches[arch].audit || prog[2] != (Instruction{Code: opRet, K: retKillProcess}) {
				t.Fatalf("%s: the program does not open with the architecture check:\n%s", arch, prog)
			}
			if last := prog[len(prog)-1]; last != (Instruction{Code: opRet, K: retAllow}) {
				t.Errorf("%s: the program does not end allowing: %+v", arch, last)
			}
			for i, in := range prog {
				switch in.Code {
				case opJeq, opJge, opJset:
					if i+1+int(in.Jt) >= len(prog) || i+1+int(in.Jf) >= len(prog) {
						t.Errorf("%s: pc %d jumps past the end", arch, i)
					}
				case opLdAbs, opRet:
					if in.Jt != 0 || in.Jf != 0 {
						t.Errorf("%s: pc %d carries jump offsets", arch, i)
					}
				default:
					t.Errorf("%s: pc %d has opcode %#x", arch, i, in.Code)
				}
			}
		}
	}
}

// A call under any other architecture is killed before its number is read,
// and so is an x32 call on amd64.
func TestArchitectureCheck(t *testing.T) {
	for _, arch := range archNames {
		prog := mustProgram(t, testCommand(true), arch)
		for _, foreign := range []uint32{0x40000003 /* i386 */, 0xc000003e, 0xc00000b7, 0x40000028 /* arm */, 0} {
			if foreign == arches[arch].audit {
				continue
			}
			if got := run(t, prog, call{arch: foreign, nr: 0}); got != retKillProcess {
				t.Errorf("%s: arch %#x gave %s", arch, foreign, retName(got))
			}
		}
		if got := run(t, prog, call{arch: arches[arch].audit, nr: 0}); got != retAllow {
			t.Errorf("%s: call 0 gave %s", arch, retName(got))
		}
	}
	prog := mustProgram(t, testCommand(true), "amd64")
	for _, n := range []uint32{x32Bit /* read */, x32Bit | 1 /* write */, x32Bit | 59 /* execve */, 0xffffffff} {
		if got := run(t, prog, call{arch: arches["amd64"].audit, nr: n}); got != retKillProcess {
			t.Errorf("amd64: x32 call %#x gave %s", n, retName(got))
		}
	}
	// arm64 has no x32: a high number is simply not in the table.
	if got := run(t, mustProgram(t, testCommand(true), "arm64"), call{arch: arches["arm64"].audit, nr: x32Bit}); got != retAllow {
		t.Errorf("arm64: %#x gave %s", x32Bit, retName(got))
	}
}

// Each call the profile names gets its answer; common calls are allowed.
func TestCommandProfileDecisions(t *testing.T) {
	const (
		eperm  = retErrno | errEPERM
		enosys = retErrno | errENOSYS
	)
	refused := []string{"ptrace", "process_vm_readv", "process_vm_writev", "kcmp", "pidfd_getfd",
		"unshare", "setns", "mount", "umount2", "open_tree", "open_tree_attr", "move_mount", "fsopen", "fsconfig",
		"fsmount", "fspick", "mount_setattr", "pivot_root", "chroot", "bpf", "perf_event_open", "userfaultfd",
		"io_uring_setup", "io_uring_enter", "io_uring_register", "init_module", "finit_module", "delete_module",
		"kexec_load", "kexec_file_load", "keyctl", "add_key", "request_key"}
	allowedNr := map[string][]uint32{
		"amd64": {0 /* read */, 1 /* write */, 2 /* open */, 59 /* execve */, 57 /* fork */, 58 /* vfork */, 257 /* openat */, 434 /* pidfd_open */, 53 /* socketpair */, 42 /* connect */},
		"arm64": {63 /* read */, 64 /* write */, 56 /* openat */, 221 /* execve */, 434 /* pidfd_open */, 199 /* socketpair */, 203 /* connect */},
	}
	type tc struct {
		name string
		args [6]uint64
		want uint32
	}
	for _, arch := range archNames {
		audit := arches[arch].audit
		for _, network := range []bool{false, true} {
			prog := mustProgram(t, testCommand(network), arch)
			cases := []tc{
				{"clone", [6]uint64{0x3d0f00}, retAllow},                 // a pthread
				{"clone", [6]uint64{0x01200011}, retAllow},               // fork
				{"clone", [6]uint64{0x10000000 | 17}, eperm},             // CLONE_NEWUSER
				{"clone", [6]uint64{0x00020000}, eperm},                  // CLONE_NEWNS
				{"clone", [6]uint64{0x40000000}, eperm},                  // CLONE_NEWNET
				{"clone", [6]uint64{0x02000000}, eperm},                  // CLONE_NEWCGROUP
				{"clone", [6]uint64{0x1_0000_0000 | 0x3d0f00}, retAllow}, // high word ignored, as the kernel does
				{"clone3", [6]uint64{}, enosys},
				{"seccomp", [6]uint64{1}, eperm},      // SECCOMP_SET_MODE_FILTER
				{"seccomp", [6]uint64{0}, eperm},      // SECCOMP_SET_MODE_STRICT
				{"seccomp", [6]uint64{2}, retAllow},   // SECCOMP_GET_ACTION_AVAIL
				{"seccomp", [6]uint64{3}, retAllow},   // SECCOMP_GET_NOTIF_SIZES
				{"prctl", [6]uint64{22, 2}, eperm},    // PR_SET_SECCOMP
				{"prctl", [6]uint64{38, 1}, retAllow}, // PR_SET_NO_NEW_PRIVS
				{"prctl", [6]uint64{15}, retAllow},    // PR_SET_NAME
			}
			// With the network on, inet, inet6 and route netlink only; off,
			// no socket at all. Unix sockets never.
			onOnly := func() uint32 {
				if network {
					return retAllow
				}
				return eperm
			}
			cases = append(cases,
				tc{"socket", [6]uint64{afInet, 1}, onOnly()},
				tc{"socket", [6]uint64{afInet, 2 /* SOCK_DGRAM */}, onOnly()},
				tc{"socket", [6]uint64{afInet6, 2}, onOnly()},
				tc{"socket", [6]uint64{afNetlink, 3, netlinkRoute}, onOnly()},
				tc{"socket", [6]uint64{0x1_0000_0000 | afInet, 1}, onOnly()}, // high word ignored, as the kernel does
				tc{"socket", [6]uint64{1 /* AF_UNIX */, 1}, eperm},
				tc{"socket", [6]uint64{1 /* AF_UNIX */, 2}, eperm},
				tc{"socket", [6]uint64{17 /* AF_PACKET */, 3}, eperm},
				tc{"socket", [6]uint64{40 /* AF_VSOCK */, 1}, eperm},
				tc{"socket", [6]uint64{afNetlink, 3, 9 /* NETLINK_AUDIT */}, eperm},
				tc{"socket", [6]uint64{afNetlink, 3, 15 /* NETLINK_KOBJECT_UEVENT */}, eperm},
				tc{"socket", [6]uint64{38 /* AF_ALG */, 5}, eperm},
				tc{"socket", [6]uint64{0x1_0000_0000 | 1, 1}, eperm})
			for _, name := range refused {
				cases = append(cases, tc{name, [6]uint64{}, eperm})
			}
			for _, c := range cases {
				if got := run(t, prog, call{arch: audit, nr: nr(t, arch, c.name), args: c.args}); got != c.want {
					t.Errorf("%s network=%v: %s%v gave %s, want %s", arch, network, c.name, c.args[:3], retName(got), retName(c.want))
				}
			}
			for _, n := range allowedNr[arch] {
				if got := run(t, prog, call{arch: audit, nr: n}); got != retAllow {
					t.Errorf("%s: call %d gave %s", arch, n, retName(got))
				}
			}
		}
	}
}

// A rule naming a call the table does not have refuses to build, rather than
// silently filtering nothing.
func TestUnknownSyscallRefused(t *testing.T) {
	for _, arch := range archNames {
		_, err := build(arch, []rule{{name: "no_such_call", errno: errEPERM}})
		if err == nil || !strings.Contains(err.Error(), `no system call "no_such_call" on `+arch) {
			t.Errorf("%s: %v", arch, err)
		}
	}
	// Calls only one architecture has.
	if _, err := build("arm64", []rule{{name: "open", errno: errEPERM}}); err == nil {
		t.Error("arm64 built a rule for open, which it does not have")
	}
	if _, err := testCommand(false).Program("riscv64"); err == nil || !strings.Contains(err.Error(), "riscv64") {
		t.Errorf("riscv64: %v", err)
	}
}

func TestAssemblerRefusesBadJumps(t *testing.T) {
	a := newAsm()
	a.jump(opJeq, 1, "nowhere", "")
	if _, err := a.done(); err == nil {
		t.Error("an undefined label assembled")
	}
	a = newAsm()
	a.mark("back")
	a.ret(retAllow)
	a.jump(opJeq, 1, "back", "")
	if _, err := a.done(); err == nil {
		t.Error("a backward jump assembled")
	}
	a = newAsm()
	a.jump(opJeq, 1, "far", "")
	for range 256 {
		a.ret(retAllow)
	}
	a.mark("far")
	a.ret(retAllow)
	if _, err := a.done(); err == nil {
		t.Error("a jump over 256 instructions assembled")
	}
}

// The programs are pinned: any change to a table, a rule or the assembler
// shows here and needs a deliberate -update (and a new Profile if the rules
// changed).
func TestGoldenPrograms(t *testing.T) {
	for _, arch := range archNames {
		for _, network := range []string{"off", "on"} {
			prog := mustProgram(t, testCommand(network == "on"), arch)
			got := prog.String() + "sha256 " + prog.Digest() + "\n"
			path := filepath.Join("testdata", "command-"+arch+"-network-"+network+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s differs from the built program; run go test -update after checking the change\n%s", path, got)
			}
		}
	}
}

func TestDescribe(t *testing.T) {
	for _, arch := range archNames {
		off := testCommand(false).describe(arch)
		on := testCommand(true).describe(arch)
		for _, want := range []string{"seccomp command/3 on linux/" + arch, "kills other architectures",
			"process tampering (ptrace, process_vm_readv, process_vm_writev, kcmp, pidfd_getfd)",
			"clone with namespace flags", "clone3 with ENOSYS", "pivot_root, chroot", "io_uring_register",
			"kexec_file_load", "keyrings (keyctl, add_key, request_key)", "prctl PR_SET_SECCOMP", "sha256 ",
			"signals to Abhed (kill of Abhed's process 4242, its group or every process, tkill of it, tgkill of it, " +
				"rt_sigqueueinfo of it, rt_tgsigqueueinfo of it, pidfd_send_signal to any process)"} {
			if !strings.Contains(off, want) || !strings.Contains(on, want) {
				t.Errorf("%s: description lacks %q:\n%s\n%s", arch, want, off, on)
			}
		}
		if !strings.Contains(off, "network off") || !strings.Contains(off, "socket for every family; socketpair allowed") {
			t.Errorf("%s off: %s", arch, off)
		}
		if !strings.Contains(on, "network on") || !strings.Contains(on, "socket families other than inet, inet6 and route netlink, unix included") {
			t.Errorf("%s on: %s", arch, on)
		}
		if strings.Contains(off, " and x32") != (arch == "amd64") {
			t.Errorf("%s: x32 wording: %s", arch, off)
		}
	}
	if d := testCommand(false).Describe(); d == "" {
		t.Error("empty description")
	}
}
