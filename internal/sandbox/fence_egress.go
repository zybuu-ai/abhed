//go:build linux

package sandbox

import (
	"context"
	"fmt"
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
	dir   string
	caDir string // the inspection CA bundle's folder, when the policy inspects TLS
	err   error
	eg    *egressState
}

// fenceEgressSpec is the proxy socket a launcher's relay joins and the address
// it listens on in the command's network namespace.
type fenceEgressSpec struct {
	Sock   string `json:"sock"`
	Listen string `json:"listen"`
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
	}
	return fc, nil
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
