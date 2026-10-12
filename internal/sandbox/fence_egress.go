//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// fenceEgress is a fenced session's egress proxy: on loopback for
// landlock_only, on a Unix socket for each command's namespace relay otherwise.
type fenceEgress struct {
	once  sync.Once
	proxy *egress.Proxy
	dir   string
	err   error
	eg    *egressState
}

// fenceEgressSpec is the proxy socket a launcher's relay joins and the address
// it listens on in the command's network namespace.
type fenceEgressSpec struct {
	Sock   string `json:"sock"`
	Listen string `json:"listen"`
}

// start starts the session's proxy once.
func (e *fenceEgress) start(pol *egress.Policy) error {
	e.once.Do(func() {
		e.eg = &egressState{session: "fence"}
		e.proxy, e.err = egress.Start(egress.Options{Policy: pol, Record: e.eg.route, Resolve: egressResolve})
		if e.err != nil {
			return
		}
		// Outside every write grant: a command cannot put another socket in its place.
		if e.dir, e.err = os.MkdirTemp("", "abhed-fence-egress-"); e.err == nil {
			e.err = e.proxy.ListenUnix(filepath.Join(e.dir, "proxy.sock"))
		}
		if e.err != nil {
			_ = e.proxy.Close()
		}
	})
	return e.err
}

// call is a credential for the launch's call, revoked when ctx ends, with
// the variables that name the proxy.
func (e *fenceEgress) call(ctx context.Context, pol *egress.Policy) ([]string, error) {
	if err := e.start(pol); err != nil {
		return nil, fmt.Errorf("the egress proxy: %w", err)
	}
	l := LaunchOf(ctx)
	c, err := e.proxy.Issue(l.CallID)
	if err != nil {
		return nil, fmt.Errorf("an egress credential: %w", err)
	}
	e.eg.remember(l)
	context.AfterFunc(ctx, c.End)
	return c.Env(), nil
}

// spec is the relay's part of the launch.
func (e *fenceEgress) spec() *fenceEgressSpec {
	return &fenceEgressSpec{Sock: filepath.Join(e.dir, "proxy.sock"), Listen: e.proxy.Addr().String()}
}

// port is the proxy's port, the one TCP port a command may connect to.
func (e *fenceEgress) port() uint16 { return e.proxy.Addr().Port() }

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
