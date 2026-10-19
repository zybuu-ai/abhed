//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// fenceEgress is a fenced session's egress proxy, reached by each call its own
// way: a loopback port in landlock_only, else a socket for its relay.
type fenceEgress struct {
	once  sync.Once
	proxy *egress.Proxy
	// dnsOnce starts the session's resolver for mount mode; dns is set once
	// it serves, and names then resolve inside the command's namespace.
	dnsOnce sync.Once
	dns     bool
	dir     string
	caDir   string // the inspection CA bundle's folder, when the policy inspects TLS
	err     error
	eg      *egressState
}

// fenceEgressSpec is the proxy socket a launcher's relay joins and the address
// it listens on in the command's network namespace.
type fenceEgressSpec struct {
	Sock   string `json:"sock"`
	Listen string `json:"listen"`
	// DNS is the resolver's socket the launcher serves 127.0.0.1:53 from,
	// and ResolvConf the file bound over /etc/resolv.conf; "" for none.
	DNS        string `json:"dns,omitempty"`
	ResolvConf string `json:"resolv_conf,omitempty"`
}

// fenceCall is what one command is given to reach the proxy.
type fenceCall struct {
	env []string
	// port is the one TCP port the command may connect to: its call's own in
	// landlock_only, the relay's in mount mode.
	port  uint16
	relay *fenceEgressSpec // mount mode only
	caDir string           // the CA bundle's folder, which the command may read, or ""
}

// start starts the session's proxy once.
func (e *fenceEgress) start(pol *egress.Policy) error {
	e.once.Do(func() {
		e.eg = &egressState{session: "fence"}
		e.proxy, e.err = egressStart(egress.Options{Policy: pol, Record: e.eg.route, Resolve: egressResolve, IPv6Loopback: true})
		if e.err != nil {
			return
		}
		// Outside every write grant: a command cannot put another socket in its place.
		e.dir, e.err = os.MkdirTemp("", "abhed-fence-egress-")
		if e.err == nil && e.proxy.Inspecting() {
			// A folder of its own, granted whole, so a rotated bundle renamed in is readable.
			e.caDir = filepath.Join(e.dir, "ca")
			if e.err = os.Mkdir(e.caDir, 0o700); e.err == nil {
				var path string
				if path, e.err = e.proxy.WriteCABundle(e.caDir); e.err == nil {
					e.proxy.SetCAPath(path)
				}
			}
		}
		if e.err != nil {
			_ = e.proxy.Close()
			if e.dir != "" {
				_ = os.RemoveAll(e.dir)
			}
		}
	})
	return e.err
}

// call is a credential for the launch's call, revoked when ctx ends, with a
// way in of its own: its relay's socket, or a port only its Landlock domain reaches.
func (e *fenceEgress) call(ctx context.Context, pol *egress.Policy, mounts bool) (fenceCall, error) {
	if err := e.start(pol); err != nil {
		return fenceCall{}, fmt.Errorf("the egress proxy: %w", err)
	}
	l := LaunchOf(ctx)
	c, err := e.proxy.Issue(l.CallID)
	if err != nil {
		return fenceCall{}, fmt.Errorf("an egress credential: %w", err)
	}
	if mounts {
		err = c.ListenUnix(filepath.Join(e.dir, "c"+strconv.FormatUint(callSeq.Add(1), 36)+".sock"))
	} else {
		err = c.ListenLoopback(true)
	}
	if err != nil {
		c.End()
		return fenceCall{}, fmt.Errorf("the call's own proxy listener: %w", err)
	}
	e.eg.remember(l)
	context.AfterFunc(ctx, c.End)
	fc := fenceCall{env: c.Env(), port: c.Port(), caDir: e.caDir}
	if mounts {
		fc.port = e.proxy.Addr().Port()
		fc.relay = &fenceEgressSpec{Sock: c.Socket(), Listen: e.proxy.Addr().String()}
		if e.startDNS() {
			fc.relay.DNS, fc.relay.ResolvConf = filepath.Join(e.dir, "dns.sock"), filepath.Join(e.dir, "resolv.conf")
		}
	}
	return fc, nil
}

// fenceResolvConf is resolvConf over TCP: the fence's seccomp filter lets a
// command make inet stream sockets alone, so lookups must not use UDP.
const fenceResolvConf = "# Written by Abhed: names resolve through the session's egress policy.\nnameserver 127.0.0.1\nsearch .\noptions timeout:2 attempts:2 use-vc\n"

// startDNS serves the session's resolver once, for commands in a network
// namespace of their own, and reports whether it does. Where /etc/resolv.conf
// is not a file to bind over, names stay unresolved, as before.
func (e *fenceEgress) startDNS() bool {
	e.dnsOnce.Do(func() {
		if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err != nil || !regularFile(real) {
			slog.Warn("egress: /etc/resolv.conf is not a file to bind over, so names will not resolve inside the fence")
			return
		}
		if err := e.proxy.ListenDNS(filepath.Join(e.dir, "dns.sock")); err != nil {
			slog.Warn("egress: the fence's resolver did not start, so names will not resolve inside it", "err", err)
			return
		}
		if err := os.WriteFile(filepath.Join(e.dir, "resolv.conf"), []byte(fenceResolvConf), 0o644); err != nil { // #nosec G306 -- read inside the fence
			slog.Warn("egress: the fence's resolv.conf was not written, so names will not resolve inside it", "err", err)
			return
		}
		e.dns = true
	})
	return e.dns
}

// regularFile reports whether path is a regular file.
func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// close stops the proxy and removes its socket's folder.
func (e *fenceEgress) close() error {
	e.once.Do(func() { e.err = errEgressClosed })
	if e.proxy == nil {
		return nil
	}
	err := e.proxy.Close()
	if e.dir != "" {
		_ = os.RemoveAll(e.dir)
	}
	return err
}
