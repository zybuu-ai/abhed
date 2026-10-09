package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Helper runs each instance through a separate program built on Apple's
// Virtualization.framework. The program is not part of Abhed: it has to be
// code-signed with the com.apple.security.virtualization entitlement.
//
// The helper protocol, version 1:
//
//	<helper> check
//	    prints one JSON line {"protocol":1,"supported":bool,"reason":"…"};
//	    supported is true only when the framework can run a VM here and the
//	    helper holds the entitlement.
//	<helper> run --config <file>
//	    boots the VM the file describes (helperConfig), with no network
//	    device and no shared folders; listens on vsock_socket (mode 0600)
//	    and bridges "CONNECT <port>\n" to that guest vsock port, answering
//	    "OK <port>\n"; joins a guest connection to a port in guest_ports to
//	    a new connection to that port's socket, and refuses the guest any
//	    other port; writes the guest console to stdout; exits when the VM
//	    stops, and stops the VM when it is killed. It refuses a key it
//	    does not know.
type Helper struct {
	// Path is the helper's absolute path; empty until one is installed.
	Path string `json:"path,omitempty"`
}

// HelperProtocol is the helper protocol version this driver speaks.
const HelperProtocol = 1

const defaultHelperBootArgs = "console=hvc0 root=/dev/vda ro init=" + GuestInit

// ErrNoHelper is why the macOS backend is unavailable until a signed helper
// is installed.
var ErrNoHelper = errors.New("the macOS microVM backend needs a Virtualization.framework helper, " +
	"a separate program that is not part of this release: it must be code-signed with the " +
	"com.apple.security.virtualization entitlement and set as helper.path. Until then the microvm " +
	"driver is unavailable on macOS; the process tier is unchanged")

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

type helperCheck struct {
	Protocol  int    `json:"protocol"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
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
	h.probe = func() error { return probeHelper(h.h.Path) }
	return h, nil
}

func (h *helper) name() string { return "vz-helper" }

func (h *helper) check() error { return h.probe() }

// runHelperCheck runs `<path> check` and reads its answer.
func runHelperCheck(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, path, "check").Output()
	if err != nil {
		return fmt.Errorf("%s check: %w", path, err)
	}
	var c helperCheck
	if err := json.Unmarshal(out, &c); err != nil {
		return fmt.Errorf("%s check: unreadable answer: %w", path, err)
	}
	if c.Protocol != HelperProtocol {
		return fmt.Errorf("%s speaks helper protocol %d, not %d", path, c.Protocol, HelperProtocol)
	}
	if !c.Supported {
		return fmt.Errorf("%s cannot run a VM here: %s", path, c.Reason)
	}
	return nil
}

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
	m, err := startMachine(cmd, sock, lns, nil, cleanup)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	return m, nil
}
