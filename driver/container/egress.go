package container

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/egress"
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
}

// egressRefusal says why the allowlist cannot be held here, or "".
func egressRefusal(eng engineInfo) string {
	if hostOS != "linux" {
		return "sandbox.network allowlist needs the engine on this Linux host: here containers run in a VM, " +
			"which a host Unix socket cannot reach, so the sandbox is not created rather than given the network"
	}
	if eng.remote {
		return "sandbox.network allowlist needs the engine on this host, reached over a local Unix socket; " +
			"DOCKER_HOST or CONTAINER_HOST points elsewhere, so the sandbox is not created"
	}
	return ""
}

// startEgress starts a proxy for one container under pol and places the
// relay and the proxy's socket in a folder of their own. With SELinux the
// relay is a copy, since relabelling the folder would relabel a link's
// original too.
func startEgress(id string, pol *egress.Policy, record func(string, map[string]any) error, selinux bool) (*egressState, error) {
	exe, err := relayExecutable()
	if err != nil {
		return nil, driver.Errorf(driver.ErrUnavailable, "finding this binary to run as the egress relay: %v", err)
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
	if err == nil {
		err = placeRelay(exe, filepath.Join(dir, "relay"), selinux)
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
	return &egressState{proxy: p, dir: dir, listen: p.Addr().String()}, nil
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
	return errors.Join(e.proxy.Close(), os.RemoveAll(e.dir))
}
