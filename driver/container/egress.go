package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/stdiorelay"
)

// hostOS is runtime.GOOS; tests set it to check another host's arguments.
var hostOS = runtime.GOOS

// egressRelayArg makes the relay binary act as the relay.
const egressRelayArg = egress.RelayArg

// relayExecutable is the binary bound in as the relay: this one, which runs
// the relay before anything else (internal/sandbox's init). Tests replace it.
var relayExecutable = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// egressState is one container's egress proxy, on the host. The container
// has no network; its init is the relay, listening on the container's own
// loopback at the proxy's port and joining each connection to the proxy's
// Unix socket, bound in from the host.
type egressState struct {
	proxy  *egress.Proxy
	dir    string
	listen string
	// stdio is set where the engine is in a VM or elsewhere: the relay is
	// the operator's Linux build, and reaches the proxy over an exec's stdio.
	stdio bool
	// stopRelay ends the stdio relay's exec; relayDone closes once it has.
	stopRelay context.CancelFunc
	relayDone chan struct{}
}

// stdioEgress reports whether the allowlist must reach the proxy over an
// exec's stdio: the engine is not on this Linux host, behind a local socket.
func stdioEgress(eng engineInfo) bool { return hostOS != "linux" || eng.remote }

// egressRefusal says why the allowlist cannot be held here, or "".
func egressRefusal(eng engineInfo, relayBinary string) string {
	if !stdioEgress(eng) || relayBinary != "" {
		return ""
	}
	if hostOS != "linux" {
		return "sandbox.network allowlist with the engine in a VM needs a Linux build of Abhed to run inside as the relay " +
			"(control_plane.<engine>.relay_binary), which is not set, so the sandbox is not created rather than given the network"
	}
	return "sandbox.network allowlist with DOCKER_HOST or CONTAINER_HOST pointing elsewhere needs a Linux build of Abhed " +
		"to run inside as the relay (control_plane.<engine>.relay_binary), which is not set, so the sandbox is not created"
}

// startEgress starts a proxy for one container under pol and places the
// relay and the proxy's socket in a folder of their own. With SELinux the
// relay is a copy, since relabelling the folder would relabel a link's
// original too.
func startEgress(id string, pol *egress.Policy, record func(string, map[string]any) error, selinux, stdio bool) (*egressState, error) {
	exe := ""
	if !stdio {
		var err error
		if exe, err = relayExecutable(); err != nil {
			return nil, driver.Errorf(driver.ErrUnavailable, "finding this binary to run as the egress relay: %v", err)
		}
	}
	route := func(ev egress.Event) {
		payload := ev.Payload()
		payload["sandbox_id"] = id
		if record == nil {
			slog.Warn("egress decision dropped: the driver has no record", "sandbox_id", id,
				"host", ev.Host, "decision", string(ev.Decision), "rule", ev.Rule)
			return
		}
		_ = record(egress.EventName, payload)
	}
	p, err := egress.Start(egress.Options{Policy: pol, Record: route})
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "starting the egress proxy: %v", err)
	}
	dir, err := os.MkdirTemp("", "abhed-egress-")
	if err == nil {
		// The container's user, which may not be this one, connects to the socket.
		err = os.Chmod(dir, 0o755) // #nosec G302 -- holds only the relay and the proxy's socket, which needs a call's token
	}
	if err == nil && !stdio {
		err = placeRelay(exe, filepath.Join(dir, "relay"), selinux)
	}
	if err == nil && p.Inspecting() {
		// Where the container sees it: in the folder bound in, or bound on its own.
		err = os.WriteFile(filepath.Join(dir, "ca.pem"), p.CABundle(), 0o644) // #nosec G306 -- public certificates, read inside the container
		p.SetCAPath(egressCA)
	}
	if err == nil {
		err = p.ListenUnix(filepath.Join(dir, "proxy.sock"))
	}
	if err == nil {
		err = os.Chmod(filepath.Join(dir, "proxy.sock"), 0o666) // #nosec G302 -- every request still needs a call's token
	}
	if err != nil {
		_ = p.Close()
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		return nil, driver.Errorf(driver.ErrUnavailable, "the egress proxy's socket: %v", err)
	}
	return &egressState{proxy: p, dir: dir, listen: p.Addr().String(), stdio: stdio}, nil
}

// placeRelay links exe to dst, or copies it where a link will not do.
func placeRelay(exe, dst string, copyIt bool) error {
	if !copyIt {
		if err := os.Link(exe, dst); err == nil {
			return nil
		}
	}
	src, err := os.Open(exe) // #nosec G304 -- this binary
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) // #nosec G302 G304 -- the relay must be executable inside
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// call issues a credential for one command; end revokes it.
func (e *egressState) call(callID string) (env []string, end func(), err error) {
	c, err := e.proxy.Issue(callID)
	if err != nil {
		return nil, nil, fmt.Errorf("an egress credential: %w", err)
	}
	return c.Env(), c.End, nil
}

func (e *egressState) close() error {
	if e == nil {
		return nil
	}
	if e.stopRelay != nil {
		e.stopRelay()
		<-e.relayDone
	}
	return errors.Join(e.proxy.Close(), os.RemoveAll(e.dir))
}

// relayReady bounds the wait for the stdio relay to listen inside.
const relayReady = 30 * time.Second

// startStdioRelay runs the relay inside in's container, over a long-lived
// exec, and joins each connection it takes to the proxy. It returns once
// the relay listens; one that never does refuses the sandbox. The exec is
// run again if it ends while the sandbox runs.
func (d *Driver) startStdioRelay(in *instance) error {
	e := in.egress
	ctx, cancel := context.WithCancel(context.Background())
	e.stopRelay, e.relayDone = cancel, make(chan struct{})
	ready := make(chan error, 1)
	// Not the sandbox's user: its commands could otherwise write the relay's frames.
	args := []string{"exec", "-i", "--user", stdiorelay.User(in.uid), in.name, egressRelay, stdiorelay.Arg, e.listen}
	sock := filepath.Join(e.dir, "proxy.sock")
	go func() {
		defer close(e.relayDone)
		first := true
		for ctx.Err() == nil {
			err := d.runStdioRelay(ctx, args, sock, func() {
				if first {
					first = false
					ready <- nil
				}
			})
			if first {
				ready <- fmt.Errorf("the relay ended before it listened: %w", err)
				return
			}
			if ctx.Err() == nil {
				slog.Warn("the egress relay inside a sandbox ended; starting it again", "sandbox_id", in.spec.ID, "error", err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-e.relayDone
			return driver.Errorf(driver.ErrUnavailable, "the egress relay inside the container: %v", err)
		}
		return nil
	case <-time.After(relayReady):
		cancel()
		<-e.relayDone
		return driver.Errorf(driver.ErrUnavailable, "the egress relay inside the container did not listen within %s", relayReady)
	}
}

// runStdioRelay runs one exec of the relay until it or ctx ends.
func (d *Driver) runStdioRelay(ctx context.Context, args []string, sock string, ready func()) error {
	return stdiorelay.Run(d.r.command(ctx, args), func() (net.Conn, error) { return net.Dial("unix", sock) }, ready)
}
