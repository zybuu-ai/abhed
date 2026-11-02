// Package loopport is the loopback port range Abhed's own listeners on a
// Mac take their ports from when they do not name one: the egress proxies
// and each call's own proxy port, port forwards, abhed serve, the metrics
// listener and abhed egress-proxy on port 0, and the sign-in callback. A
// debug session on the macOS process tier never reaches this range, so it
// reaches no Abhed listener of any engine on the Mac but one whose port a
// person named (docs/guide/31-debugging.md). The range holds 256 ports by
// default, shared by every Abhed on the Mac.
//
// The range is the same for every Abhed on the Mac: 47600 to 47855 unless
// ABHED_LOOPBACK_PORTS names another ("lo-hi"), which every Abhed process
// there must then be given. Elsewhere the rule does not apply: a Linux
// sandbox has a network namespace of its own, and listeners take any port.
package loopport

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Env names the range ("lo-hi") in place of the default.
const Env = "ABHED_LOOPBACK_PORTS"

// The default range.
const (
	DefaultLow  = 47600
	DefaultHigh = 47855
)

// goos is the system the rule applies to; tests set it.
var goos = runtime.GOOS

// Applies reports whether listeners here take their ports from the range.
func Applies() bool { return goos == "darwin" }

// Range is the reserved range, inclusive: the default, or Env's when it
// names a valid one of at least 16 ports above 1024. One Env names that is
// not valid is warned about, once, and the default used: every Abhed on
// the Mac must use the same range, and a process that silently fell back
// would put its listeners where another's debug session may reach.
func Range() (lo, hi int) {
	if v := os.Getenv(Env); v != "" {
		a, b, ok := strings.Cut(v, "-")
		l, e1 := strconv.Atoi(strings.TrimSpace(a))
		h, e2 := strconv.Atoi(strings.TrimSpace(b))
		if ok && e1 == nil && e2 == nil && l > 1024 && h <= 65535 && h-l >= 15 {
			return l, h
		}
		warnOnce.Do(func() {
			slog.Warn("ignoring "+Env+": it must be lo-hi, at least 16 ports above 1024; using the default, which every Abhed on this Mac must then use too",
				"value", v, "default", strconv.Itoa(DefaultLow)+"-"+strconv.Itoa(DefaultHigh))
		})
	}
	return DefaultLow, DefaultHigh
}

var warnOnce sync.Once

// ListenAddr is net.Listen("tcp", addr), but for any port on a Mac (a port
// of "0", "00" or empty, as net reads them): a port of the range then, on
// addr's host, so a listener a person or a configuration asked for on any
// port (a port forward, abhed serve on :0, a metrics or egress-proxy
// listener) is never one a debug session on the Mac may reach. A port named
// explicitly is taken as named.
func ListenAddr(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !AnyPort(port) || !Applies() {
		return net.Listen("tcp", addr)
	}
	return Listen(host)
}

// AnyPort reports whether port, as written in an address, asks for any
// port: empty, or a number that is 0.
func AnyPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n == 0
}

// In reports whether port is in the reserved range, where the rule applies.
func In(port int) bool {
	if !Applies() {
		return false
	}
	lo, hi := Range()
	return port >= lo && port <= hi
}

// ErrExhausted is no port of the range free.
var ErrExhausted = errors.New("loopport: no loopback port free in Abhed's reserved range")

func exhausted() error {
	lo, hi := Range()
	return fmt.Errorf("%w %d-%d; free some, or give every Abhed on this Mac another range in %s", ErrExhausted, lo, hi, Env)
}

// Listen listens on host (an address or a name, looked up once; empty is
// every address) at a port of the range where the rule applies, else at a
// port the system picks. A failure other than a port in use or refused is
// returned as it is, not as ErrExhausted.
func Listen(host string) (net.Listener, error) {
	if !Applies() {
		return net.Listen("tcp", net.JoinHostPort(host, "0"))
	}
	at, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	lo, hi := Range()
	for _, p := range order(lo, hi) {
		a := *at
		a.Port = p
		ln, err := net.ListenTCP("tcp", &a)
		if err == nil {
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) && !errors.Is(err, syscall.EACCES) {
			return nil, err
		}
	}
	return nil, exhausted()
}

// ListenBoth listens on the same port on 127.0.0.1 and [::1], of the range
// where the rule applies; v6 false, or no IPv6 loopback, gives 127.0.0.1
// alone.
func ListenBoth(v6 bool) ([]net.Listener, error) {
	if !Applies() {
		return listenPair(0, v6)
	}
	lo, hi := Range()
	for _, p := range order(lo, hi) {
		if lns, err := listenPair(p, v6); err == nil {
			return lns, nil
		}
	}
	return nil, exhausted()
}

func listenPair(port int, v6 bool) ([]net.Listener, error) {
	for range 20 {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		if !v6 || !ipv6Loopback() {
			return []net.Listener{ln}, nil
		}
		got := ln.Addr().(*net.TCPAddr).Port
		ln6, err := net.Listen("tcp", net.JoinHostPort("::1", strconv.Itoa(got)))
		if err == nil {
			return []net.Listener{ln, ln6}, nil
		}
		_ = ln.Close()
		if port != 0 {
			return nil, err // that port is taken on [::1]
		}
	}
	return nil, errors.New("loopport: no loopback port free on both 127.0.0.1 and [::1]")
}

// FreeOutside are n loopback ports nothing listens on now, on 127.0.0.1 or
// [::1], none of them in the range: for a command that will hold them
// itself (a debug adapter's pinned ports).
func FreeOutside(n int) ([]int, error) {
	var held []net.Listener
	defer func() {
		for _, l := range held {
			_ = l.Close()
		}
	}()
	var out []int
	for tries := 0; len(out) < n; tries++ {
		if tries > 200 {
			return nil, errors.New("loopport: no loopback port free on both 127.0.0.1 and [::1] outside the reserved range")
		}
		lns, err := listenPair(0, true)
		if err != nil {
			return nil, err
		}
		held = append(held, lns...)
		if p := lns[0].Addr().(*net.TCPAddr).Port; !In(p) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ipv6Loopback reports whether [::1] can be listened on at all.
func ipv6Loopback() bool {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// order is the range's ports in a random order, so engines spread out.
func order(lo, hi int) []int {
	out := make([]int, 0, hi-lo+1)
	for p := lo; p <= hi; p++ {
		out = append(out, p)
	}
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] }) // #nosec G404 -- spreading, not secrecy
	return out
}
