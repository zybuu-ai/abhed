// Package dapbridge carries a debug adapter's unix socket over stdio, inside
// a session's sandbox: the adapter (dlv dap) listens on a socket there, and
// the engine, which may be outside a VM the container runs in, reaches it
// only through the bridge's stdin and stdout. It does nothing else.
package dapbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Arg makes an Abhed binary, such as the configured relay_binary, the
// bridge: `abhed Arg unix:PATH`.
const Arg = "__abhed_dap_bridge"

// Name is the bridge's file name for a Linux architecture, as the release
// archive and the Studio build place it beside the engine.
func Name(arch string) string { return "abhed-dapbridge-linux-" + arch }

// Pinned are the release build's checksums of the bridges, set with -ldflags
// -X; empty in a development build, whose bridge is taken unpinned.
var (
	PinnedAmd64 string
	PinnedArm64 string
)

func pinned(arch string) string {
	switch arch {
	case "amd64":
		return PinnedAmd64
	case "arm64":
		return PinnedArm64
	}
	return ""
}

// ErrMissing is a bridge that is not beside the engine.
var ErrMissing = errors.New("the debug bridge is not installed beside the engine")

// Find is the bridge for arch beside the running engine, checked against its
// pinned checksum; pinnedOK says whether a pin held (false in a development
// build, where none is set).
func Find(arch string) (path string, pinnedOK bool, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return check(filepath.Join(filepath.Dir(exe), Name(arch)), arch)
}

func check(path, arch string) (string, bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("%w: %s", ErrMissing, path)
	}
	want := pinned(arch)
	if want == "" {
		return path, false, nil
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the bridge beside the engine
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return "", false, fmt.Errorf("the debug bridge %s does not match the checksum this engine was built with (sha256 %s, want %s)", path, got, want)
	}
	return path, true, nil
}

// Arch is the Linux architecture a sandbox's container runs: the host's, as
// a podman or Docker machine on this host runs.
func Arch() string { return runtime.GOARCH }

// Run dials addr (unix:PATH), waiting up to wait for the adapter to make the
// socket, and copies stdin to it and it to stdout until either side ends.
func Run(addr string, in io.Reader, out io.Writer, wait time.Duration) error {
	path, ok := strings.CutPrefix(addr, "unix:")
	if !ok || path == "" {
		return fmt.Errorf("dapbridge: %q is not unix:PATH", addr)
	}
	var conn net.Conn
	var err error
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		if conn, err = net.Dial("unix", path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dapbridge: the adapter's socket %s: %w", path, err)
		}
	}
	defer func() { _ = conn.Close() }()
	// Input ending closes the socket's writing half, and the adapter's answers
	// still come back; the bridge ends when the adapter's side does.
	go func() {
		_, _ = io.Copy(conn, in)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	_, err = io.Copy(out, conn)
	return err
}

// Main runs the bridge with os.Args[1] as its address.
func Main(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: abhed-dapbridge unix:PATH")
		return 2
	}
	if err := Run(args[0], os.Stdin, os.Stdout, 30*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func init() {
	if len(os.Args) == 3 && os.Args[1] == Arg {
		os.Exit(Main(os.Args[2:]))
	}
}
