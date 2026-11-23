package app

import (
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/zybuu-ai/abhed/config"
)

// serveSignInChoice is serve's first-start question, asked only in a
// terminal: a server with no sign-in that others can reach runs commands
// as this OS user for anyone who reaches it. It returns the address to serve
// on, or ok false to stop without serving. A run nobody can ask serves as
// configured, as it always has.
func serveSignInChoice(cfg config.Config, addr string, interactive bool, p *prompter) (string, bool) {
	if !interactive || (cfg.Auth.Mode != "" && cfg.Auth.Mode != "none") || loopbackAddr(addr) {
		return addr, true
	}
	out := p.out
	fmt.Fprintf(out, "\nThis server has no sign-in (auth.mode is none), and %s is reachable from other machines:\n", addr)
	fmt.Fprintln(out, "anyone who reaches it can run commands as you.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  1) Set up sign-in first: shows the two commands, and serves nothing")
	fmt.Fprintln(out, "  2) Serve on this machine only (127.0.0.1), without sign-in")
	fmt.Fprintln(out, "  3) Serve as configured, without sign-in")
	for range 3 {
		pick, err := p.line("\nChoose", "1")
		if err != nil {
			return addr, false
		}
		switch pick {
		case "1":
			serveSignInSteps(out)
			return addr, false
		case "2":
			_, port, _ := net.SplitHostPort(addr)
			if port == "" {
				port = strings.TrimPrefix(addr, ":")
			}
			return net.JoinHostPort("127.0.0.1", port), true
		case "3":
			return addr, true
		}
		fmt.Fprintf(out, "  %q is not one of the choices.\n", pick)
	}
	return addr, false
}

func serveSignInSteps(out io.Writer) {
	fmt.Fprintln(out, "\nTo serve with local accounts:")
	fmt.Fprintln(out, `  1. In the workspace's .abhed/config.json (or the managed one): {"auth":{"mode":"local","users_file":"<a path outside every workspace>"}}`)
	fmt.Fprintln(out, "  2. abhed user add <name> -admin    (prints a password to change at first sign-in)")
	fmt.Fprintln(out, "Then abhed serve again. Docs: TLS and command-line sign-in, and Signing in.")
}

// loopbackAddr reports whether a listen address is this machine's only.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
