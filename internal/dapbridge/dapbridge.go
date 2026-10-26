// Package dapbridge is the helper's debug bridge: inside a sandbox, it joins
// its own stdin and stdout to a debug adapter listening on the sandbox's
// loopback, so a debugger outside talks DAP over the engine's exec stream
// and the sandbox needs no published port.
package dapbridge

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// Arg is the helper's first argument for the bridge:
//
//	abhed-helper __abhed_dap_bridge --connect 127.0.0.1:PORT [--wait 10s]
const Arg = "__abhed_dap_bridge"

// Run bridges stdin and stdout to the adapter at --connect, waiting up to
// --wait for it to listen. It returns the process's exit code.
func Run(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet(Arg, flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("connect", "", "the adapter's loopback address, host:port")
	wait := fs.Duration("wait", 10*time.Second, "how long to wait for the adapter to listen")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := checkLoopback(*addr); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", Arg, err)
		return 2
	}
	var conn net.Conn
	var err error
	for deadline := time.Now().Add(*wait); ; time.Sleep(100 * time.Millisecond) {
		if conn, err = net.DialTimeout("tcp", *addr, time.Second); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: the adapter at %s did not answer: %v\n", Arg, *addr, err)
		return 1
	}
	defer func() { _ = conn.Close() }()
	// The end of stdin half-closes the connection so the adapter's last
	// answer still arrives; the adapter closing ends the bridge.
	go func() {
		_, _ = io.Copy(conn, stdin)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	if _, err := io.Copy(stdout, conn); err != nil && !errors.Is(err, net.ErrClosed) {
		fmt.Fprintf(stderr, "%s: %v\n", Arg, err)
		return 1
	}
	return 0
}

// checkLoopback refuses an address off the sandbox's own loopback: the
// bridge reaches only an adapter inside it.
func checkLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" || strings.ContainsAny(port, " /") {
		return fmt.Errorf("--connect is host:port, not %q", addr)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("--connect reaches only the loopback, not %q", host)
	}
	return nil
}

// init serves the bridge when a binary that links this package, the engine
// on a Linux host or the helper, is run with Arg.
func init() {
	if len(os.Args) > 1 && os.Args[1] == Arg {
		os.Exit(Run(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
}
