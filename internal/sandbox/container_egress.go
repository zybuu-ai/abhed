package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/zybuu-ai/abhed/internal/dapbridge" // the debug bridge, when this binary is bound into a container
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/enginecli"
	"github.com/zybuu-ai/abhed/internal/stdiorelay"
)

// relayInside is where the relay binary is bound in a container.
const relayInside = "/run/abhed-relay"

// caDirInside holds the inspection CA bundle in a container, bound whole so a
// rotated bundle renamed in is seen; caInside is the bundle.
const (
	caDirInside = "/run/abhed-ca"
	caInside    = caDirInside + "/ca.pem"
)

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

// helperArch is the shipped helper's architecture, the engine's own: an
// engine's VM runs the host's. Tests set it.
var helperArch = runtime.GOARCH

// containerRelay is the Linux relay binary run in a container: this one on a
// Linux host, else the configured relay_binary; "" with why when none.
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
	if c.policy.RelayBinary == "" && EngineOnAnotherHost() {
		return "", RemoteHelperRefusal + ", so the " + string(c.Tier()) + " tier is not used rather than given the network"
	}
	if c.policy.RelayBinary == "" {
		// The helper shipped beside the engine, verified against its pin.
		if helper, why := BundledHelper(helperArch); helper != "" {
			return helper, ""
		} else if why != "" {
			return "", "the egress proxy with the engine in a VM or on another host needs a Linux build of Abhed, or the abhed-helper " +
				"shipped with it, to run inside each container as the relay (control_plane.<engine>.relay_binary); " + why + ", so the " +
				string(c.Tier()) + " tier is not used rather than given the network"
		}
	}
	return c.policy.RelayBinary, ""
}

// containerProxy starts the sandbox's egress proxy once, with a private folder
// for its calls' sockets.
func (c *Container) containerProxy() (*egress.Proxy, *egressState, error) {
	c.egOnce.Do(func() {
		c.eg = &egressState{session: "container"}
		c.egProxy, c.egErr = egressStart(egress.Options{Policy: c.policy.Egress, Record: c.eg.route})
		if c.egErr != nil {
			return
		}
		c.egDir, c.egErr = os.MkdirTemp("", "abhed-egr-")
		if c.egErr == nil && c.egProxy.Inspecting() {
			// Bound into each command's container, where its clients are pointed.
			ca := filepath.Join(c.egDir, "ca")
			if c.egErr = os.Mkdir(ca, 0o755); c.egErr == nil { // #nosec G301 -- public certificates, read inside the container
				_, c.egErr = c.egProxy.WriteCABundle(ca)
			}
			c.egProxy.SetCAPath(caInside)
		}
		if c.egErr != nil {
			_ = c.egProxy.Close()
			if c.egDir != "" {
				_ = os.RemoveAll(c.egDir)
			}
		}
	})
	return c.egProxy, c.eg, c.egErr
}

// egressRun runs argv in a networkless container whose only way out is a
// relay to the egress proxy over an exec's stdio. The container takes p's
// mounts and extra run arguments; argv gets env besides the proxy's. The func
// returned removes the container and ends the call, at once.
func (c *Container) egressRun(ctx context.Context, cwd, prefix string, p Policy, extra, env, argv []string) (*exec.Cmd, func()) {
	relay, why := c.containerRelay()
	if relay == "" {
		return &exec.Cmd{Err: errors.New("sandbox: the command was not run: " + why)}, nil
	}
	proxy, routes, err := c.containerProxy()
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: the egress proxy: %w", err)}, nil
	}
	// Counted from here until set up or given up, so Close stops it and waits.
	key := new(int)
	setupCtx, stopSetup := context.WithCancel(ctx)
	defer stopSetup()
	c.egCallsMu.Lock()
	if c.egClosed {
		c.egCallsMu.Unlock()
		return &exec.Cmd{Err: errEgressClosed}, nil
	}
	if c.egSetupStop == nil {
		c.egSetupStop = map[*int]context.CancelFunc{}
	}
	c.egSetupStop[key] = stopSetup
	c.egSetups.Add(1)
	c.egCallsMu.Unlock()
	defer func() {
		c.egCallsMu.Lock()
		delete(c.egSetupStop, key)
		c.egCallsMu.Unlock()
		c.egSetups.Done()
	}()
	l := LaunchOf(ctx)
	call, err := proxy.Issue(l.CallID)
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: an egress credential: %w", err)}, nil
	}
	// The relay of this command's container alone reaches the call's own
	// socket, so what arrives there is the call's, whatever it carries.
	sock := filepath.Join(c.egDir, "c"+strconv.FormatUint(callSeq.Add(1), 36)+".sock")
	if err := call.ListenUnix(sock); err != nil {
		call.End()
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: the call's own proxy listener: %w", err)}, nil
	}
	routes.remember(l)
	args, err := c.runArgsWith(ctx, cwd, p, false)
	if err != nil {
		call.End()
		return &exec.Cmd{Err: err}, nil
	}
	// Once per process, before its first: what a killed Abhed left.
	sweptNote(c.SweepServers(setupCtx))
	name := containerName(prefix)
	// Attached, with the relay bound read-only and labelled as this
	// process's. Its main process reads its input, held open by this process:
	// if Abhed ends, even killed, the input closes, the container exits and
	// --rm removes it; a sweep at the next start removes any left still.
	run := append([]string{"run", "--rm", "-i"}, args[3:]...)
	run = append(append(append(run, extra...), egressOwnerArgs()...), "-v", relay+":"+relayInside+":ro")
	if proxy.Inspecting() {
		run = append(run, "-v", filepath.Join(c.egDir, "ca")+":"+caDirInside+":ro")
	}
	run = append(run, "--name", name, SandboxImage(), "/bin/sh", "-c", lifelineScript)
	startCtx, cancel := context.WithTimeout(setupCtx, relayReady)
	defer cancel()
	life, err := c.startLifeline(startCtx, name, run)
	end := func() {
		call.End()
		if life != nil {
			life.cut()
		}
		rmCtx, cancel := context.WithTimeout(context.Background(), engineWait)
		defer cancel()
		_ = enginecli.Command(rmCtx, c.runtime, c.rmArgs(name)...).Run()
		if life != nil {
			life.wait(engineWait)
		}
	}
	if err != nil {
		end()
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: starting its container: %w", err)}, nil
	}
	listen := "127.0.0.1:" + strconv.Itoa(int(relayPort(proxy.Addr().Port())))
	relayCtx, stopRelay := context.WithCancel(context.WithoutCancel(ctx))
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		rc := c.engineCmd(relayCtx, "exec", "-i", "--user", stdiorelay.User(os.Getuid()), name, relayInside, stdiorelay.Arg, listen)
		done <- stdiorelay.Run(rc, func() (net.Conn, error) { return net.Dial("unix", sock) }, func() { close(ready) })
	}()
	stop := func() {
		stopRelay()
		end()
	}
	select {
	case <-ready:
	case err := <-done:
		stop()
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: its egress relay ended: %w", err)}, nil
	case <-startCtx.Done():
		stop()
		return &exec.Cmd{Err: errors.New("sandbox: the command was not run: its egress relay did not start in time")}, nil
	}
	// In a private file, not an argument any process can list nor the CLI's
	// own environment, whose proxy settings its client would follow.
	envFile, removeEnv, err := EnvFile(append(call.Env(), env...))
	if err != nil {
		stop()
		return &exec.Cmd{Err: fmt.Errorf("sandbox: the command was not run: its proxy settings: %w", err)}, nil
	}
	// Once, and waited for by any second caller: the call's end, its
	// command's cancel, and Close may all ask.
	stopAll := sync.OnceFunc(func() {
		stop()
		removeEnv()
		c.egCallsMu.Lock()
		delete(c.egCalls, key)
		c.egCallsMu.Unlock()
	})
	// Registered under the lock Close takes: a call set up as the sandbox
	// closes is torn down here rather than left standing.
	c.egCallsMu.Lock()
	if c.egClosed {
		c.egCallsMu.Unlock()
		stopAll()
		return &exec.Cmd{Err: errEgressClosed}, nil
	}
	if c.egCalls == nil {
		c.egCalls = map[*int]func(){}
	}
	c.egCalls[key] = stopAll
	c.egCallsMu.Unlock()
	context.AfterFunc(ctx, stopAll)
	execArgs := []string{"exec", "-i", "-w", cwd}
	if uid, gid := os.Getuid(), os.Getgid(); uid > 0 {
		execArgs = append(execArgs, "--user", fmt.Sprintf("%d:%d", uid, gid))
	}
	execArgs = append(append(execArgs, "--env-file", envFile, "-e", "ABHED_SANDBOX="+string(c.Tier()), name), argv...)
	cmd := c.engineCmd(ctx, execArgs...)
	cmd.Cancel = func() error {
		stopAll()
		return cmd.Process.Kill()
	}
	return cmd, stopAll
}

// lifelineScript is an egress container's main process: it ends when its
// input, held by Abhed, closes.
const lifelineScript = "trap 'exit 0' TERM; while read -r _; do :; done; exit 0"

// egressOwnerLabel marks an egress container with the Abhed process that
// started it, as serverOwnerLabel does a server's.
const egressOwnerLabel = "abhed.egress.owner"

// egressOwnerArgs label an egress container as this process's.
func egressOwnerArgs() []string {
	if v := ownerOf(os.Getpid()); v != "" {
		return []string{"--label", egressOwnerLabel + "=" + v}
	}
	return nil
}

// lifeline is the engine client an egress container runs attached to.
type lifeline struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
	out   *limitedBuffer
}

// cut closes the container's input, which ends it.
func (l *lifeline) cut() { _ = l.stdin.Close() }

// wait waits up to d for the engine client to end, then kills it.
func (l *lifeline) wait(d time.Duration) {
	select {
	case <-l.done:
	case <-time.After(d):
		_ = l.cmd.Process.Kill()
		<-l.done
	}
}

// startLifeline starts the container attached and waits until it runs.
func (c *Container) startLifeline(ctx context.Context, name string, run []string) (*lifeline, error) {
	// Not tied to ctx: the container lives as long as the call, past setup.
	cmd := c.engineCmd(context.WithoutCancel(ctx), run...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out := &limitedBuffer{left: 8 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	l := &lifeline{cmd: cmd, stdin: stdin, done: make(chan struct{}), out: out}
	go func() { _ = cmd.Wait(); close(l.done) }()
	for {
		got, _ := c.engineCmd(ctx, "inspect", "--format", "{{.State.Running}}", name).Output()
		if strings.TrimSpace(string(got)) == "true" {
			return l, nil
		}
		select {
		case <-l.done:
			return l, fmt.Errorf("the engine ended: %s", strings.TrimSpace(out.String()))
		case <-ctx.Done():
			return l, fmt.Errorf("it did not start in time: %s", strings.TrimSpace(out.String()))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// limitedBuffer keeps the first left bytes written to it.
type limitedBuffer struct {
	mu   sync.Mutex
	b    bytes.Buffer
	left int
}

func (w *limitedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if k := min(len(p), w.left); k > 0 {
		w.b.Write(p[:k])
		w.left -= k
	}
	return len(p), nil
}

// engineCmd is the engine's CLI with args, given the host's environment,
// which it needs to find the engine; only -e flags reach the container.
func (c *Container) engineCmd(ctx context.Context, args ...string) *exec.Cmd {
	return enginecli.Command(ctx, c.runtime, args...)
}

// closeEgress ends every call's container still standing, waiting for its
// removal, then stops the sandbox's proxy and removes its calls' sockets.
func (c *Container) closeEgress() error {
	c.egOnce.Do(func() { c.egErr = errEgressClosed })
	c.egCallsMu.Lock()
	c.egClosed = true
	ends := make([]func(), 0, len(c.egCalls))
	for _, f := range c.egCalls {
		ends = append(ends, f)
	}
	for _, stop := range c.egSetupStop {
		stop()
	}
	c.egCallsMu.Unlock()
	// Calls still starting give up and remove what they started, each bounded
	// by the engine's own wait; then the standing ones go.
	c.egSetups.Wait()
	// Together, each bounded by the engine's own wait, so Close takes about
	// as long as one removal however many calls stand.
	var wg sync.WaitGroup
	for _, f := range ends {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}
	wg.Wait()
	var err error
	if c.egProxy != nil {
		err = c.egProxy.Close()
	}
	if c.egDir != "" {
		err = errors.Join(err, os.RemoveAll(c.egDir))
	}
	return err
}
