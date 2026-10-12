package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/stdiorelay"
)

// relayInside is where the relay binary is bound in a container.
const relayInside = "/run/abhed-relay"

// relayReady bounds how long a command waits for its container's relay.
const relayReady = 30 * time.Second

// egressPrefix names the container a command under the allowlist runs in;
// ForwardEnv finds it to pass a command's variables.
var egressPrefix = "abhed-egr-"

// relayPort is the port the relay listens on inside, the proxy's own, which
// the call's proxy variables name; a test on the host moves it.
var relayPort = func(proxy uint16) uint16 { return proxy }

// containerHostOS is runtime.GOOS; tests set it to check another host.
var containerHostOS = runtime.GOOS

// containerRelay is the Linux binary run inside a container as the relay:
// this one on a Linux host whose engine is local, else the operator's build
// named in control_plane.<engine>.relay_binary; "" with why when there is none.
func (c *Container) containerRelay() (string, string) {
	remote := os.Getenv("DOCKER_HOST") != "" || os.Getenv("CONTAINER_HOST") != ""
	if containerHostOS == "linux" && !remote {
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		if err != nil {
			return "", "finding this binary to run as the egress relay: " + err.Error()
		}
		return exe, ""
	}
	if c.policy.RelayBinary == "" {
		return "", "sandbox.network allowlist with the engine in a VM or on another host needs a Linux build of Abhed to run " +
			"inside each container as the relay (control_plane.<engine>.relay_binary), which is not set, so the " + string(c.Tier()) +
			" tier is not used rather than given the network"
	}
	return c.policy.RelayBinary, ""
}

// containerProxy starts the sandbox's egress proxy once.
func (c *Container) containerProxy() (*egress.Proxy, *egressState, error) {
	c.egOnce.Do(func() {
		c.eg = &egressState{session: "container"}
		c.egProxy, c.egErr = egress.Start(egress.Options{Policy: c.policy.Egress, Record: c.eg.route})
	})
	return c.egProxy, c.eg, c.egErr
}

// egressCommand runs command in a container of its own whose only way out
// is the sandbox's egress proxy: the container starts with no network and
// idles, the relay inside listens on its loopback at the proxy's port and
// carries each connection over a long-lived exec's stdio to the proxy, and
// the command runs by a second exec with the call's proxy variables. The
// container goes when ctx ends.
func (c *Container) egressCommand(ctx context.Context, cwd, command string) *exec.Cmd {
	relay, why := c.containerRelay()
	if relay == "" {
		return &exec.Cmd{Err: errors.New("sandbox: the command was not run: " + why)}
	}
	proxy, routes, err := c.containerProxy()
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: the egress proxy: %w", err)}
	}
	l := LaunchOf(ctx)
	call, err := proxy.Issue(l.CallID)
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: an egress credential: %w", err)}
	}
	routes.remember(l)
	args, err := c.runArgs(ctx, cwd)
	if err != nil {
		call.End()
		return &exec.Cmd{Err: err}
	}
	name := containerName(egressPrefix)
	// Detached, with the relay bound read-only; it idles until removed.
	run := append([]string{"run", "--rm", "-d"}, args[3:]...)
	run = append(run, "-v", relay+":"+relayInside+":ro", "--name", name, Image,
		"/bin/sh", "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait $!; done")
	end := func() {
		call.End()
		rmCtx, cancel := context.WithTimeout(context.Background(), engineWait)
		defer cancel()
		_ = exec.CommandContext(rmCtx, c.runtime, "rm", "-f", name).Run() // #nosec G204 -- the configured engine and a name this process made
	}
	startCtx, cancel := context.WithTimeout(ctx, relayReady)
	defer cancel()
	if out, err := c.engineCmd(startCtx, run...).CombinedOutput(); err != nil {
		end()
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: starting its container: %w: %s", err, strings.TrimSpace(string(out)))}
	}
	listen := "127.0.0.1:" + strconv.Itoa(int(relayPort(proxy.Addr().Port())))
	relayCtx, stopRelay := context.WithCancel(context.WithoutCancel(ctx))
	ready := make(chan struct{})
	done := make(chan error, 1)
	upstream := proxy.Addr().String()
	go func() {
		rc := c.engineCmd(relayCtx, "exec", "-i", name, relayInside, stdiorelay.Arg, listen)
		done <- stdiorelay.Run(rc, func() (net.Conn, error) { return net.Dial("tcp", upstream) }, func() { close(ready) })
	}()
	stop := func() {
		stopRelay()
		end()
	}
	select {
	case <-ready:
	case err := <-done:
		stop()
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: its egress relay ended: %w", err)}
	case <-startCtx.Done():
		stop()
		return &exec.Cmd{Err: errors.New("sandbox: the command was not run: its egress relay did not start in time")}
	}
	context.AfterFunc(ctx, stop)
	execArgs := []string{"exec", "-i", "-w", cwd}
	if uid, gid := os.Getuid(), os.Getgid(); uid > 0 {
		execArgs = append(execArgs, "--user", fmt.Sprintf("%d:%d", uid, gid))
	}
	for _, kv := range call.Env() {
		execArgs = append(execArgs, "-e", kv)
	}
	execArgs = append(execArgs, "-e", "ABHED_SANDBOX="+string(c.Tier()), name, "/bin/sh", "-c", command)
	cmd := c.engineCmd(ctx, execArgs...)
	cmd.Cancel = func() error {
		stop()
		return cmd.Process.Kill()
	}
	return cmd
}

// engineCmd is the engine's CLI with args, given the host's environment,
// which it needs to find the engine; only -e flags reach the container.
func (c *Container) engineCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.runtime, args...) // #nosec G204 -- the configured engine
	cmd.Env = os.Environ()
	return cmd
}

// closeEgress stops the sandbox's proxy.
func (c *Container) closeEgress() error {
	c.egOnce.Do(func() { c.egErr = errEgressClosed })
	if c.egProxy != nil {
		return c.egProxy.Close()
	}
	return nil
}
