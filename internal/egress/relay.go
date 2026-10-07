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

// DNSArg, after the relay's socket and address, names the resolver's socket
// and the uid and gid the command runs as.
const DNSArg = "--dns"

// ResolverAddr is where the relay serves DNS inside the sandbox, as the generated resolv.conf names it.
const ResolverAddr = "127.0.0.1:53"

// relayDNS listens for DNS inside the sandbox and passes queries to sock, with
// the proxy credential the command was given, so the proxy knows the call.
func relayDNS(sock string) error {
	cred := Credential(os.Getenv("HTTP_PROXY"))
	if cred == "" {
		return errors.New("no proxy credential in HTTP_PROXY")
	}
	udp, err := net.ListenPacket("udp", ResolverAddr)
	if err != nil {
		return err
	}
	tcp, err := net.Listen("tcp", ResolverAddr)
	if err != nil {
		_ = udp.Close()
		return err
	}
	go ServeDNS(udp, tcp, sock, cred)
	return nil
}

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

// RunRelay joins the command's proxy address to the proxy's socket inside the sandbox and runs
// the command as its child. args: socket, listen address, [DNSArg dns-socket uid gid], "--", argv.
func RunRelay(args []string) int {
	var attr *syscall.SysProcAttr
	dns := len(args) >= 6 && args[2] == DNSArg
	if dns {
		// Bind port 53 with the sandbox's grant, then let nothing reach the relay.
		var err error
		if attr, err = nestedUser(args[4], args[5]); err == nil {
			if err = relayDNS(args[3]); err == nil {
				err = undumpable()
			}
		}
		// Not nested, the command execs as root: the bounding set goes too.
		if err == nil && attr == nil {
			err = dropCaps(true)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed egress relay: the resolver: %v\n", err)
			return 126
		}
		args = append(args[:2:2], args[6:]...)
	}
	if len(args) < 4 || args[2] != "--" {
		fmt.Fprintln(os.Stderr, "abhed egress relay: usage: socket addr ["+DNSArg+" dns-socket uid gid] -- command...")
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
	cmd.SysProcAttr = attr
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
	// Writing the nested command's id maps needed the relay's capabilities; they go now.
	if err := dropCapsAfter(dns, attr); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintf(os.Stderr, "abhed egress relay: dropping capabilities: %v\n", err)
		return 126
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

// dropCapsAfter drops what the relay still holds once a nested command has started.
func dropCapsAfter(dns bool, attr *syscall.SysProcAttr) error {
	if !dns || attr == nil {
		return nil
	}
	return dropCaps(false)
}
