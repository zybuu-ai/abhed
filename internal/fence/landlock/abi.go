package landlock

import (
	"errors"
	"fmt"
	"strings"
)

// ABI is the Landlock ABI version a kernel implements; 0 means it has none.
type ABI int

// MinABI is the oldest ABI the fence accepts: before ABI 3 (Linux 6.2) a
// command could truncate a file it may not open.
const MinABI ABI = 3

// Thresholds for the features the fence uses beyond the minimum.
const (
	networkABI ABI = 4 // TCP bind and connect rules
	scopingABI ABI = 6 // signal and abstract-socket scoping
)

// Errors that a refusal wraps, for callers that need to tell them apart.
var (
	// ErrUnsupported: the kernel lacks Landlock or a feature the spec needs.
	ErrUnsupported = errors.New("landlock: not supported here")
	// ErrRoot: the fence does not confine a process running as root.
	ErrRoot = errors.New("landlock: refused for root")
	// ErrSpec: the spec cannot be applied as written.
	ErrSpec = errors.New("landlock: spec refused")
)

// Kernel UAPI access rights (include/uapi/linux/landlock.h). They are fixed
// by the kernel ABI, and a Linux test holds them equal to x/sys/unix's.
const (
	fsExecute    uint64 = 1 << 0
	fsWriteFile  uint64 = 1 << 1
	fsReadFile   uint64 = 1 << 2
	fsReadDir    uint64 = 1 << 3
	fsRemoveDir  uint64 = 1 << 4
	fsRemoveFile uint64 = 1 << 5
	fsMakeChar   uint64 = 1 << 6
	fsMakeDir    uint64 = 1 << 7
	fsMakeReg    uint64 = 1 << 8
	fsMakeSock   uint64 = 1 << 9
	fsMakeFifo   uint64 = 1 << 10
	fsMakeBlock  uint64 = 1 << 11
	fsMakeSym    uint64 = 1 << 12
	fsRefer      uint64 = 1 << 13
	fsTruncate   uint64 = 1 << 14
	fsIoctlDev   uint64 = 1 << 15

	netBindTCP    uint64 = 1 << 0
	netConnectTCP uint64 = 1 << 1

	scopeAbstractUnix uint64 = 1 << 0
	scopeSignal       uint64 = 1 << 1
)

// Rights by grant kind. A rule on a file, not a folder, may carry only
// fileRights.
const (
	fsRightsV1 = fsExecute | fsWriteFile | fsReadFile | fsReadDir | fsRemoveDir | fsRemoveFile |
		fsMakeChar | fsMakeDir | fsMakeReg | fsMakeSock | fsMakeFifo | fsMakeBlock | fsMakeSym
	fileRights   = fsExecute | fsWriteFile | fsReadFile | fsTruncate | fsIoctlDev
	readRights   = fsReadFile | fsReadDir
	execRights   = readRights | fsExecute
	deviceRights = readRights | fsWriteFile | fsTruncate | fsIoctlDev
	netTCP       = netBindTCP | netConnectTCP
)

var fsRightNames = []struct {
	bit  uint64
	name string
}{
	{fsExecute, "execute"}, {fsWriteFile, "write_file"}, {fsReadFile, "read_file"},
	{fsReadDir, "read_dir"}, {fsRemoveDir, "remove_dir"}, {fsRemoveFile, "remove_file"},
	{fsMakeChar, "make_char"}, {fsMakeDir, "make_dir"}, {fsMakeReg, "make_reg"},
	{fsMakeSock, "make_sock"}, {fsMakeFifo, "make_fifo"}, {fsMakeBlock, "make_block"},
	{fsMakeSym, "make_sym"}, {fsRefer, "refer"}, {fsTruncate, "truncate"}, {fsIoctlDev, "ioctl_dev"},
}

// kernels is the first Linux release with each ABI, for refusal messages.
var kernels = map[ABI]string{1: "5.13", 2: "5.19", 3: "6.2", 4: "6.7", 5: "6.10", 6: "6.12", 7: "6.15"}

// Kernel is the first Linux release with this ABI, or "" when not known.
func (a ABI) Kernel() string { return kernels[a] }

func (a ABI) named() string {
	if k := a.Kernel(); k != "" {
		return fmt.Sprintf("ABI %d (Linux %s)", int(a), k)
	}
	return fmt.Sprintf("ABI %d", int(a))
}

// Check refuses an ABI older than MinABI, naming the kernel that has it.
func (a ABI) Check() error {
	if a <= 0 {
		return fmt.Errorf("%w: this kernel has no Landlock; %s or later is needed", ErrUnsupported, MinABI.named())
	}
	if a < MinABI {
		return fmt.Errorf("%w: this kernel's Landlock is ABI %d; %s or later is needed, so that a command cannot truncate a file it may not open",
			ErrUnsupported, int(a), MinABI.named())
	}
	return nil
}

// handledFS is every filesystem right the ABI knows; each is refused wherever
// no rule grants it.
func (a ABI) handledFS() uint64 {
	if a <= 0 {
		return 0
	}
	r := fsRightsV1
	if a >= 2 {
		r |= fsRefer
	}
	if a >= 3 {
		r |= fsTruncate
	}
	if a >= 5 {
		r |= fsIoctlDev
	}
	return r
}

// FSRights names the filesystem access rights the ABI controls.
func (a ABI) FSRights() []string {
	h := a.handledFS()
	var out []string
	for _, r := range fsRightNames {
		if h&r.bit != 0 {
			out = append(out, r.name)
		}
	}
	return out
}

// Network reports whether the ABI has TCP bind and connect rules.
func (a ABI) Network() bool { return a >= networkABI }

// Scoping reports whether the ABI scopes signals and abstract Unix sockets
// to the domain.
func (a ABI) Scoping() bool { return a >= scopingABI }

// scopes are the scoping bits the ABI enforces; none before ABI 6.
func (a ABI) scopes() uint64 {
	if !a.Scoping() {
		return 0
	}
	return scopeSignal | scopeAbstractUnix
}

// Describe is the one-line account for abhed doctor and the record.
func (a ABI) Describe() string {
	if a <= 0 {
		return "Landlock: not available"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Landlock %s: files (%s)", a.named(), strings.Join(a.FSRights(), ", "))
	if a.Network() {
		b.WriteString("; TCP bind and connect rules")
	} else {
		fmt.Fprintf(&b, "; no TCP rules (%s)", networkABI.named())
	}
	if a.Scoping() {
		b.WriteString("; signals and abstract sockets scoped to the domain")
	} else {
		fmt.Fprintf(&b, "; no signal scoping (%s)", scopingABI.named())
	}
	if err := a.Check(); err != nil {
		fmt.Fprintf(&b, "; too old for the fence, which needs %s", MinABI.named())
	}
	return b.String()
}
