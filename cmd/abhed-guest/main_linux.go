// Command abhed-guest is the agent inside a microvm driver guest. Built
// static (CGO_ENABLED=0 GOOS=linux), it is installed in the guest's root
// filesystem as /sbin/abhed-guest and started by the kernel as init.
//
// As init it mounts the guest's filesystems, mounts the workspace image at
// /workspace, and starts itself again as the agent, which listens on vsock
// and runs the host's commands as an unprivileged user. Init only reaps;
// if the agent ends, or any step of setting up fails, the guest powers off,
// so the host sees the VM exit rather than a guest it cannot reach.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/driver/microvm/guest"
)

// The user commands run as; the workspace is given to it.
const cmdUID, cmdGID = 1000, 1000

const workspace = "/workspace"

func main() {
	switch {
	case os.Getpid() == 1:
		initMain()
	case len(os.Args) == 2 && os.Args[1] == "agent":
		// The agent returns only when it can no longer accept calls.
		fmt.Fprintln(os.Stderr, "abhed-guest agent:", agent())
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "abhed-guest runs only as a microVM guest's init")
		os.Exit(2)
	}
}

func powerOff() {
	unix.Sync()
	// Firecracker and the macOS helper end the VM when the guest reboots.
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	select {}
}

func initMain() {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "abhed-guest init:", err)
		powerOff()
	}
	cmd := exec.Command("/proc/self/exe", "agent")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "abhed-guest init: starting the agent:", err)
		powerOff()
	}
	agentPID := cmd.Process.Pid
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if pid == agentPID || (err != nil && err != unix.EINTR) {
			fmt.Fprintln(os.Stderr, "abhed-guest init: the agent ended")
			powerOff()
		}
	}
}

type mount struct {
	src, dst, fstype string
	flags            uintptr
	data             string
}

func setup() error {
	mounts := []mount{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_RDONLY, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID, "mode=0755"},
		{"devpts", "/dev/pts", "devpts", unix.MS_NOSUID | unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=0755"},
	}
	for _, m := range mounts {
		if m.dst == "/dev/pts" {
			if err := os.MkdirAll(m.dst, 0o755); err != nil {
				return err
			}
		}
		if err := unix.Mount(m.src, m.dst, m.fstype, m.flags, m.data); err != nil && err != unix.EBUSY {
			return fmt.Errorf("mounting %s: %w", m.dst, err)
		}
	}
	_ = os.Symlink("/dev/pts/ptmx", "/dev/ptmx")
	_ = unix.Sethostname([]byte("abhed-microvm"))
	if err := loopbackUp(); err != nil {
		return err
	}
	// The second drive, when the host attached one, is the workspace.
	if _, err := os.Stat("/dev/vdb"); err == nil {
		if err := unix.Mount("/dev/vdb", workspace, "ext4", unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			return fmt.Errorf("mounting the workspace: %w", err)
		}
		if err := giveTree(workspace); err != nil {
			return err
		}
	}
	return nil
}

// giveTree hands the workspace to the command user; the image keeps the
// host's owners, which mean nothing in the guest.
func giveTree(root string) error {
	return filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return unix.Lchown(p, cmdUID, cmdGID)
	})
}

// loopbackUp brings up lo, the only interface: the guest has no NIC.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_UP | unix.IFF_LOOPBACK | unix.IFF_RUNNING)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	return nil
}

func agent() error {
	dir, home := "/tmp", "/tmp"
	var tracker *guest.Tracker
	if mounted(workspace) {
		dir, home = workspace, workspace
		t, err := guest.NewTracker(workspace)
		if err != nil {
			return fmt.Errorf("reading the workspace: %w", err)
		}
		tracker = t
	}
	s := &guest.Server{
		Env: []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME=" + home, "USER=abhed", "LANG=C.UTF-8", "ABHED_SANDBOX=microvm",
		},
		Dir:        dir,
		Shell:      "/bin/sh",
		Credential: &syscall.Credential{Uid: cmdUID, Gid: cmdGID, Groups: []uint32{}},
		PowerOff:   powerOff,
		Workspace:  tracker,
		Network:    network,
		DNS:        resolver,
		CAFile:     guest.CAFile,
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vsock: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: guest.Port}); err != nil {
		return fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		return fmt.Errorf("vsock listen: %w", err)
	}
	for {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err == unix.EINTR || err == unix.ECONNABORTED {
			continue
		}
		if err != nil {
			return fmt.Errorf("vsock accept: %w", err)
		}
		go s.ServeConn(os.NewFile(uintptr(nfd), "vsock"))
	}
}

var (
	fwdMu   sync.Mutex
	fwdPort uint16
)

// network forwards 127.0.0.1:port to the host's egress broker, once. The
// host asks only for a sandbox allowed the network; otherwise nothing
// listens on the host's side and a connection to it is refused.
func network(port uint16) error {
	fwdMu.Lock()
	defer fwdMu.Unlock()
	if fwdPort != 0 {
		if port == fwdPort {
			return nil
		}
		return fmt.Errorf("already forwarding port %d", fwdPort)
	}
	if _, err := guest.Forward(port, dialHost); err != nil {
		return err
	}
	fwdPort = port
	return nil
}

func dialHost() (io.ReadWriteCloser, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: guest.EgressPort}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock-egress"), nil
}

func mounted(p string) bool {
	var a, b unix.Stat_t
	if unix.Stat(p, &a) != nil || unix.Stat(filepath.Dir(p), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}

var (
	dnsMu sync.Mutex
	dnsOn bool
)

// resolver starts, once, the resolver on 127.0.0.1:53 that passes lookups
// to the host, and points /etc/resolv.conf at it. The host asks only for a
// sandbox whose egress has a resolver.
func resolver() error {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	if dnsOn {
		return nil
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:53")
	if err != nil {
		return err
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:53")
	if err != nil {
		_ = udp.Close()
		return err
	}
	// The root filesystem is read-only: a file on /run is bound over it.
	conf := "/run/abhed-resolv.conf"
	err = os.WriteFile(conf, []byte("nameserver 127.0.0.1\n"), 0o644) // #nosec G306 -- read by every program
	if err == nil {
		err = unix.Mount(conf, "/etc/resolv.conf", "", unix.MS_BIND, "")
	}
	if err == nil {
		err = unix.Mount("", "/etc/resolv.conf", "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	}
	if err != nil {
		_ = udp.Close()
		_ = tcp.Close()
		return fmt.Errorf("pointing /etc/resolv.conf at the resolver (the root filesystem needs the file): %w", err)
	}
	go guest.ServeDNS(udp, tcp, dialHostDNS)
	dnsOn = true
	return nil
}

func dialHostDNS() (io.ReadWriteCloser, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: guest.EgressDNSPort}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock-dns"), nil
}
