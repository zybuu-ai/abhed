package egress

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// RelayArg is the first argument with which a sandbox re-executes Abhed as
// the relay in front of a command; see RunRelay.
const RelayArg = "__abhed_egress_relay"

// Relay accepts on ln and joins each connection to the proxy's unix socket
// at sock, until ln is closed.
func Relay(ln net.Listener, sock string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			up, err := net.Dial("unix", sock)
			if err != nil {
				return
			}
			defer func() { _ = up.Close() }()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); closeWrite(up); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
			<-done
			<-done
		}()
	}
}

// RunRelay is the relay process inside a sandbox's own network namespace:
// it listens on the loopback address the command's proxy variables name,
// joins each connection to the proxy's unix socket bound into the sandbox,
// and runs the command as its child, passing on its signals and its exit
// status. args are: socket, listen address, "--", argv.
func RunRelay(args []string) int {
	if len(args) < 4 || args[2] != "--" {
		fmt.Fprintln(os.Stderr, "abhed egress relay: usage: socket addr -- command...")
		return 126
	}
	sock, addr, argv := args[0], args[1], args[3:]
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed egress relay: listening on %s: %v\n", addr, err)
		return 126
	}
	go Relay(ln, sock)
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- the sandboxed command, inside its sandbox
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// A terminal's interrupt and quit reach the command through its process
	// group already, so the relay only survives them; a termination or
	// hang-up sent to the relay alone is passed on.
	tty := make(chan os.Signal, 8)
	signal.Notify(tty, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		for range tty {
		}
	}()
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "abhed egress relay: %v\n", err)
		return 127
	}
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	signal.Stop(sigs)
	signal.Stop(tty)
	_ = ln.Close()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	if err != nil {
		return 127
	}
	return 0
}
