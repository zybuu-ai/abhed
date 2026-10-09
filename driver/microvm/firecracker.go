package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
)

// Firecracker runs each instance as a Firecracker microVM started by the
// jailer, which chroots it, gives it a uid and gid of its own, puts it in a
// cgroup with limits, and applies Firecracker's seccomp filters. The driver
// must run as root for the jailer to do that.
type Firecracker struct {
	// Binary and Jailer are absolute paths to the release binaries, owned
	// by root and writable by no one else.
	Binary string `json:"binary"`
	Jailer string `json:"jailer"`
	// ChrootBase is where the jailer builds each VM's chroot; it should be
	// on the same filesystem as StateDir, so images are linked, not copied.
	ChrootBase string `json:"chroot_base,omitempty"`
	// Instance n runs as UIDBase+n and GIDBase+n, for n below Slots, so no
	// two VMs share a user. The range must be one no account uses.
	UIDBase uint32 `json:"uid_base"`
	GIDBase uint32 `json:"gid_base"`
	Slots   int    `json:"slots,omitempty"`
	// ParentCgroup is the cgroup v2 folder the jailer makes each VM's under.
	ParentCgroup string `json:"parent_cgroup,omitempty"`
	// SeccompFilter, when set, is a compiled filter used in place of
	// Firecracker's default; the filters are never turned off.
	SeccompFilter string `json:"seccomp_filter,omitempty"`
	// MaxOpenFiles is the VMM's file descriptor limit.
	MaxOpenFiles int `json:"max_open_files,omitempty"`
}

const defaultFirecrackerBootArgs = "console=ttyS0 reboot=k panic=1 pci=off init=" + GuestInit

var cgroupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type firecracker struct {
	o  *Options
	fc Firecracker
	// probe, chown and env are replaced in tests, which cannot run the
	// jailer as root.
	probe  func() error
	chown  func(path string, uid, gid int) error
	lchown func(path string, uid, gid int) error
	env    []string
}

func newFirecracker(o *Options) (*firecracker, error) {
	fc := *o.Firecracker
	if fc.ChrootBase == "" {
		fc.ChrootBase = "/srv/jailer"
	}
	if fc.Slots == 0 {
		fc.Slots = 64
	}
	if fc.ParentCgroup == "" {
		fc.ParentCgroup = "abhed-microvm"
	}
	if fc.MaxOpenFiles == 0 {
		fc.MaxOpenFiles = 2048
	}
	if o.BootArgs == "" {
		o.BootArgs = defaultFirecrackerBootArgs
	}
	for name, p := range map[string]string{
		"firecracker.binary": fc.Binary, "firecracker.jailer": fc.Jailer, "firecracker.chroot_base": fc.ChrootBase,
		"kernel": o.Kernel, "rootfs": o.Rootfs, "state_dir": o.StateDir,
	} {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("microvm: %s must be an absolute path, not %q", name, p)
		}
	}
	if fc.SeccompFilter != "" && !filepath.IsAbs(fc.SeccompFilter) {
		return nil, fmt.Errorf("microvm: firecracker.seccomp_filter must be an absolute path")
	}
	if fc.UIDBase == 0 || fc.GIDBase == 0 {
		return nil, errors.New("microvm: firecracker.uid_base and gid_base must be set to an unused range above 0")
	}
	if fc.Slots < 1 || fc.Slots > 4096 || uint64(fc.UIDBase)+uint64(fc.Slots) > 1<<31 || uint64(fc.GIDBase)+uint64(fc.Slots) > 1<<31 {
		return nil, errors.New("microvm: firecracker.slots must be 1 to 4096 and keep the uid and gid range below 2^31")
	}
	if !cgroupName.MatchString(fc.ParentCgroup) {
		return nil, fmt.Errorf("microvm: firecracker.parent_cgroup %q is not a plain folder name", fc.ParentCgroup)
	}
	o.Firecracker = &fc
	return &firecracker{o: o, fc: fc, probe: func() error { return probeFirecracker(o, fc) }, chown: os.Chown, lchown: os.Lchown}, nil
}

func (f *firecracker) name() string { return "firecracker" }

func (f *firecracker) check() error { return f.probe() }

// jailDir is the jailer's folder for one VM; root is the chroot inside it.
func (f *firecracker) jailDir(vmID string) string {
	return filepath.Join(f.fc.ChrootBase, filepath.Base(f.fc.Binary), vmID)
}

func (f *firecracker) ids(slot int) (uid, gid int) {
	return int(f.fc.UIDBase) + slot, int(f.fc.GIDBase) + slot
}

// Paths inside the chroot.
const (
	chKernel    = "/vmlinux"
	chRootfs    = "/rootfs.ext4"
	chWorkspace = "/workspace.ext4"
	chConfig    = "/vm.json"
	chSeccomp   = "/seccomp.bpf"
	chVsock     = "/v.sock"
)

// fcConfig is the VM's configuration file. It has no network-interfaces
// section: the guest gets no NIC, only vsock.
func fcConfig(b bootSpec) ([]byte, error) {
	type drive struct {
		ID       string `json:"drive_id"`
		Path     string `json:"path_on_host"`
		Root     bool   `json:"is_root_device"`
		ReadOnly bool   `json:"is_read_only"`
	}
	drives := []drive{{ID: "rootfs", Path: chRootfs, Root: true, ReadOnly: true}}
	if b.workspace != "" {
		drives = append(drives, drive{ID: "workspace", Path: chWorkspace})
	}
	return json.MarshalIndent(map[string]any{
		"boot-source": map[string]any{"kernel_image_path": chKernel, "boot_args": b.bootArgs},
		"drives":      drives,
		"machine-config": map[string]any{
			"vcpu_count": b.vcpus, "mem_size_mib": b.memMiB, "smt": false,
		},
		"vsock": map[string]any{"guest_cid": 3, "uds_path": chVsock},
	}, "", "  ")
}

// jailerArgs is the jailer's command line for one boot. The memory limit
// leaves the VMM 256 MiB above the guest's memory.
func (f *firecracker) jailerArgs(b bootSpec) []string {
	uid, gid := f.ids(b.slot)
	args := []string{
		"--id", b.vmID,
		"--exec-file", f.fc.Binary,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--chroot-base-dir", f.fc.ChrootBase,
		"--cgroup-version", "2",
		"--parent-cgroup", f.fc.ParentCgroup,
		"--cgroup", fmt.Sprintf("cpu.max=%d 100000", b.vcpus*100000),
		"--cgroup", fmt.Sprintf("memory.max=%d", int64(b.memMiB+256)<<20),
		"--cgroup", "memory.swap.max=0",
		"--cgroup", fmt.Sprintf("pids.max=%d", 64+b.vcpus),
		"--resource-limit", fmt.Sprintf("no-file=%d", f.fc.MaxOpenFiles),
		"--",
		"--config-file", chConfig,
		"--no-api",
	}
	if f.fc.SeccompFilter != "" {
		args = append(args, "--seccomp-filter", chSeccomp)
	}
	return args
}

// boot lays out the chroot, then starts the jailer.
func (f *firecracker) boot(_ context.Context, b bootSpec) (machine, error) {
	jail := f.jailDir(b.vmID)
	root := filepath.Join(jail, "root")
	uid, gid := f.ids(b.slot)
	cleanup := func() error {
		return errors.Join(os.RemoveAll(jail), removeCgroup(f.fc.ParentCgroup, b.vmID))
	}
	// A boot always starts from a fresh chroot, never one a VM wrote to.
	if err := cleanup(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	err := f.layout(b, root, uid, gid)
	// Firecracker sends the guest's connection to port P to the socket
	// "<uds_path>_P"; made here, while the chroot is still root's alone.
	var lns map[uint32]net.Listener
	if err == nil {
		lns, err = listenGuestPorts(b.guestPorts,
			func(p uint32) string { return filepath.Join(root, fmt.Sprintf("%s_%d", chVsock, p)) },
			func(p string) error { return f.lchown(p, uid, gid) })
	}
	if err == nil {
		if err = f.chown(root, uid, gid); err != nil {
			closeAll(lns)
		}
	}
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	cmd := exec.Command(f.fc.Jailer, f.jailerArgs(b)...)
	cmd.Env = append([]string{}, f.env...)
	cmd.SysProcAttr = vmmProcAttr()
	m, err := startMachine(cmd, filepath.Join(root, chVsock), lns,
		func() error { return killCgroup(f.fc.ParentCgroup, b.vmID) }, cleanup)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	return m, nil
}

// layout puts the kernel, root filesystem, workspace image, configuration
// and seccomp filter in the chroot, readable by the VM's user.
func (f *firecracker) layout(b bootSpec, root string, uid, gid int) error {
	place := []struct {
		src, dst string
		own      bool // the VM's user owns it: written by the VM, or private to it
	}{
		{b.kernel, chKernel, false},
		{b.rootfs, chRootfs, false},
	}
	if b.workspace != "" {
		place = append(place, struct {
			src, dst string
			own      bool
		}{b.workspace, chWorkspace, true})
	}
	if f.fc.SeccompFilter != "" {
		place = append(place, struct {
			src, dst string
			own      bool
		}{f.fc.SeccompFilter, chSeccomp, false})
	}
	for _, p := range place {
		dst := filepath.Join(root, p.dst)
		linked, err := linkOrCopy(p.src, dst)
		if err != nil {
			return fmt.Errorf("placing %s: %w", p.src, err)
		}
		// A shared, linked file must already be readable by everyone; a copy
		// or an instance's own file is given to the VM's user.
		if p.own || !linked {
			if err := f.chown(dst, uid, gid); err != nil {
				return err
			}
		} else if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("%s must be readable by every user to be shared with VMs", p.src)
		}
	}
	cfg, err := fcConfig(b)
	if err != nil {
		return err
	}
	path := filepath.Join(root, chConfig)
	if err := os.WriteFile(path, cfg, 0o400); err != nil {
		return err
	}
	return f.chown(path, uid, gid)
}

// linkOrCopy hard-links src at dst, or copies it when they are on
// different filesystems. It reports whether it linked.
func linkOrCopy(src, dst string) (bool, error) {
	if err := os.Link(src, dst); err == nil {
		return true, nil
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return false, err
	}
	return false, out.Close()
}
