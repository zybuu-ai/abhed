package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// Helper runs each instance through abhed-vmhelper, a separate program built
// on Apple's Virtualization.framework (macos/abhed-vmhelper). The framework
// runs only a program code-signed with the com.apple.security.virtualization
// entitlement, so the helper is a signed binary of its own; the driver checks
// that signature before it runs it.
//
// The helper protocol, version 1:
//
//	<helper> check
//	    prints one JSON line {"protocol":1,"supported":bool,"reason":"…"};
//	    supported is true only when the framework can run a VM here and the
//	    helper holds the entitlement. It exits 0 whatever the answer.
//	<helper> run --config <file>
//	    boots the VM the file describes (helperConfig), with no network
//	    device and no shared folders; listens on vsock_socket (mode 0600)
//	    and bridges "CONNECT <port>\n" to that guest vsock port, answering
//	    "OK <port>\n"; joins a guest connection to a port in guest_ports to
//	    a new connection to that port's socket, and refuses the guest any
//	    other port; writes the guest console to stdout; exits when the VM
//	    stops, and stops the VM and exits when it is killed or its standard
//	    input ends, which is how a driver that dies takes its VMs with it.
//	    It refuses a key it does not know. It exits 0 when the VM stopped
//	    (the guest powered off, or the helper stopped it), 1 when the VM
//	    could not start or stopped with an error, and 2 when it refused the
//	    command line or the configuration and ran no VM.
type Helper struct {
	// Path is the helper's absolute path. When empty, it is abhed-vmhelper
	// in the folder of the program running the driver.
	Path string `json:"path,omitempty"`
	// TeamID, when set, is the Apple Developer team whose Developer ID
	// certificate must have signed the helper. Unset, any valid signature
	// with the helper's identifier, the entitlement and the hardened
	// runtime is accepted, including an ad-hoc one made on this Mac.
	TeamID string `json:"team_id,omitempty"`
}

// HelperProtocol is the helper protocol version this driver speaks.
const HelperProtocol = 1

// HelperName is the helper's file name, and HelperIdentifier the code-signing
// identifier it must carry.
const (
	HelperName       = "abhed-vmhelper"
	HelperIdentifier = "com.zybuu.abhed.vmhelper"
)

const defaultHelperBootArgs = "console=hvc0 root=/dev/vda ro init=" + GuestInit

// ErrNoHelper is why the macOS backend is unavailable without its helper.
var ErrNoHelper = errors.New("the macOS microVM backend needs abhed-vmhelper, the Virtualization.framework " +
	"helper, code-signed with the com.apple.security.virtualization entitlement; install it beside " +
	"abhed-microvm or set helper.path. Until then the microvm driver is unavailable on macOS; the process tier is unchanged")

var teamID = regexp.MustCompile(`^[A-Z0-9]{10}$`)

type helperDisk struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only"`
}

// helperConfig is the file `run` reads.
type helperConfig struct {
	Protocol    int          `json:"protocol"`
	Kernel      string       `json:"kernel"`
	BootArgs    string       `json:"boot_args"`
	CPUs        int          `json:"cpus"`
	MemoryMiB   int          `json:"memory_mib"`
	Disks       []helperDisk `json:"disks"`
	VsockSocket string       `json:"vsock_socket"`
	GuestPorts  []helperPort `json:"guest_ports,omitempty"`
	Network     string       `json:"network"`
}

// helperPort is a vsock port the guest may connect to on the host.
type helperPort struct {
	Port   uint32 `json:"port"`
	Socket string `json:"socket"`
}

type helper struct {
	o     *Options
	h     Helper
	probe func() error
	env   []string // the helper's environment; empty but in tests
}

func newHelper(o *Options) (*helper, error) {
	if o.BootArgs == "" {
		o.BootArgs = defaultHelperBootArgs
	}
	for name, p := range map[string]string{"kernel": o.Kernel, "rootfs": o.Rootfs, "state_dir": o.StateDir} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("microvm: %s must be an absolute path, not %q", name, p)
		}
	}
	h := &helper{o: o, h: *o.Helper}
	if h.h.Path != "" && !filepath.IsAbs(h.h.Path) {
		return nil, fmt.Errorf("microvm: helper.path must be an absolute path")
	}
	if h.h.TeamID != "" && !teamID.MatchString(h.h.TeamID) {
		return nil, fmt.Errorf("microvm: helper.team_id %q is not an Apple Developer team id (ten letters and digits)", h.h.TeamID)
	}
	if h.h.Path == "" {
		h.h.Path = defaultHelperPath()
	}
	h.probe = func() error { return probeHelper(h.h.Path, h.h.TeamID) }
	return h, nil
}

// defaultHelperPath is abhed-vmhelper beside the running program, or empty
// when that program's path is unknown.
func defaultHelperPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Join(filepath.Dir(exe), HelperName)
}

func (h *helper) name() string { return "vz-helper" }

func (h *helper) check() error { return h.probe() }

func guestPortPath(run string, p uint32) string {
	return filepath.Join(run, fmt.Sprintf("g%d.sock", p))
}

func (h *helper) config(b bootSpec, sock string) helperConfig {
	disks := []helperDisk{{Path: b.rootfs, ReadOnly: true}}
	if b.workspace != "" {
		disks = append(disks, helperDisk{Path: b.workspace})
	}
	var ports []helperPort
	for _, p := range b.guestPorts {
		ports = append(ports, helperPort{Port: p, Socket: guestPortPath(filepath.Dir(sock), p)})
	}
	return helperConfig{
		Protocol: HelperProtocol, Kernel: b.kernel, BootArgs: b.bootArgs,
		CPUs: b.vcpus, MemoryMiB: b.memMiB, Disks: disks, VsockSocket: sock, GuestPorts: ports, Network: "none",
	}
}

func (h *helper) boot(_ context.Context, b bootSpec) (machine, error) {
	run := filepath.Join(b.stateDir, "run")
	sock := filepath.Join(run, "v.sock")
	// macOS limits a socket path to 104 bytes.
	if len(sock) > 100 {
		return nil, fmt.Errorf("the socket path %s is too long; use a shorter state_dir", sock)
	}
	cleanup := func() error { return os.RemoveAll(run) }
	if err := cleanup(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(run, 0o700); err != nil {
		return nil, err
	}
	cfg, err := json.MarshalIndent(h.config(b, sock), "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(run, "vm.json"), cfg, 0o600)
	}
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	lns, err := listenGuestPorts(b.guestPorts, func(p uint32) string { return guestPortPath(run, p) }, nil)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	cmd := exec.Command(h.h.Path, "run", "--config", filepath.Join(run, "vm.json"))
	cmd.Env = append([]string{}, h.env...)
	// Only this process holds the write end: when it exits, the helper's
	// stdin ends and the helper stops its VM.
	life, hold, err := os.Pipe()
	if err != nil {
		closeAll(lns)
		_ = cleanup()
		return nil, err
	}
	cmd.Stdin = life
	m, err := startMachine(cmd, sock, lns, func() error { return hold.Close() }, cleanup)
	_ = life.Close()
	if err != nil {
		_ = hold.Close()
		_ = cleanup()
		return nil, err
	}
	return m, nil
}
