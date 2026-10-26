// Package dapbridge carries a debug adapter's unix socket over stdio, inside
// a session's sandbox: the adapter (dlv dap) listens on a socket there, and
// the engine, which may be outside a VM the container runs in, reaches it
// only through the bridge's stdin and stdout. It is a mode of the engine's
// own binary, run from its bundled Linux build, and does nothing else.
package dapbridge

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// Arg makes an Abhed binary, its bundled Linux build or the configured
// relay_binary, the bridge: `abhed Arg unix:PATH`.
const Arg = "__abhed_dap_bridge"

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
		fmt.Fprintln(os.Stderr, "usage: abhed "+Arg+" unix:PATH")
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
