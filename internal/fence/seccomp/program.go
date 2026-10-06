package seccomp

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Instruction is one classic BPF instruction, laid out as the kernel's
// struct sock_filter.
type Instruction struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// Program is a seccomp filter: the instructions the kernel runs on each
// system call.
type Program []Instruction

// The few classic BPF opcodes a seccomp filter needs.
const (
	opLdAbs = 0x20 // BPF_LD | BPF_W | BPF_ABS
	opJeq   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	opJge   = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	opJset  = 0x45 // BPF_JMP | BPF_JSET | BPF_K
	opRet   = 0x06 // BPF_RET | BPF_K
)

// Offsets into struct seccomp_data; an argument's low word on a
// little-endian machine.
const (
	offNr   = 0
	offArch = 4
)

func offArg(i uint32) uint32 { return 16 + 8*i }

// Return values.
const (
	retKillProcess = 0x80000000
	retErrno       = 0x00050000
	retAllow       = 0x7fff0000
)

// Errnos, the same on amd64 and arm64.
const (
	errEPERM  = 1
	errENOSYS = 38
)

// maxInsns is the kernel's BPF_MAXINSNS.
const maxInsns = 4096

// asm assembles a program with forward jumps to named labels.
type asm struct {
	out    Program
	labels map[string]int
	refs   []jumpRef
}

type jumpRef struct {
	at     int
	jt, jf string // "" means the next instruction
}

func newAsm() *asm { return &asm{labels: map[string]int{}} }

func (a *asm) ld(off uint32) { a.out = append(a.out, Instruction{Code: opLdAbs, K: off}) }

func (a *asm) ret(k uint32) { a.out = append(a.out, Instruction{Code: opRet, K: k}) }

func (a *asm) jump(code uint16, k uint32, jt, jf string) {
	a.refs = append(a.refs, jumpRef{at: len(a.out), jt: jt, jf: jf})
	a.out = append(a.out, Instruction{Code: code, K: k})
}

func (a *asm) mark(label string) { a.labels[label] = len(a.out) }

// done resolves every jump, which must be forward and within 255.
func (a *asm) done() (Program, error) {
	if len(a.out) > maxInsns {
		return nil, fmt.Errorf("the program has %d instructions, more than the kernel's %d", len(a.out), maxInsns)
	}
	for _, r := range a.refs {
		jt, err := a.offset(r.at, r.jt)
		if err != nil {
			return nil, err
		}
		jf, err := a.offset(r.at, r.jf)
		if err != nil {
			return nil, err
		}
		a.out[r.at].Jt, a.out[r.at].Jf = jt, jf
	}
	return a.out, nil
}

func (a *asm) offset(at int, label string) (uint8, error) {
	if label == "" {
		return 0, nil
	}
	to, ok := a.labels[label]
	if !ok {
		return 0, fmt.Errorf("jump to an undefined label %q", label)
	}
	d := to - at - 1
	if d < 0 || d > 255 {
		return 0, fmt.Errorf("jump from %d to %q at %d is out of range", at, label, to)
	}
	return uint8(d), nil
}

// String disassembles the program one instruction per line: the raw fields,
// then a readable form with absolute jump targets.
func (p Program) String() string {
	var b strings.Builder
	for i, in := range p {
		fmt.Fprintf(&b, "%3d: 0x%04x %3d %3d 0x%08x  ", i, in.Code, in.Jt, in.Jf, in.K)
		switch in.Code {
		case opLdAbs:
			fmt.Fprintf(&b, "ld   [%d]", in.K)
		case opJeq, opJge, opJset:
			name := map[uint16]string{opJeq: "jeq ", opJge: "jge ", opJset: "jset"}[in.Code]
			fmt.Fprintf(&b, "%s %#x, %d, %d", name, in.K, i+1+int(in.Jt), i+1+int(in.Jf))
		case opRet:
			fmt.Fprintf(&b, "ret  %s", retName(in.K))
		default:
			b.WriteString("?")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func retName(k uint32) string {
	switch {
	case k == retAllow:
		return "ALLOW"
	case k == retKillProcess:
		return "KILL_PROCESS"
	case k&0xffff0000 == retErrno:
		switch k & 0xffff {
		case errEPERM:
			return "ERRNO(EPERM)"
		case errENOSYS:
			return "ERRNO(ENOSYS)"
		}
		return fmt.Sprintf("ERRNO(%d)", k&0xffff)
	}
	return fmt.Sprintf("%#x", k)
}

// Digest is the SHA-256 of the program as the kernel receives it, in hex.
func (p Program) Digest() string {
	h := sha256.New()
	var buf [8]byte
	for _, in := range p {
		binary.LittleEndian.PutUint16(buf[0:], in.Code)
		buf[2], buf[3] = in.Jt, in.Jf
		binary.LittleEndian.PutUint32(buf[4:], in.K)
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
