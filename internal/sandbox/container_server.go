package sandbox

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// serverPrefix names a server's container.
var serverPrefix = "abhed-mcp-"

// liveServers are the removals of the server containers this process runs,
// by their command, so a close or an exit removes each before it goes.
var liveServers sync.Map // *exec.Cmd -> func()

// ServerCleanup is what removes the container cmd, a server's command, runs
// in: synchronous and safe to call more than once; nil for any other command.
// Killing the engine's client alone leaves the container running.
func ServerCleanup(cmd *exec.Cmd) func() {
	if f, ok := liveServers.Load(cmd); ok {
		return f.(func())
	}
	return nil
}

// EndServers removes every server container this process still runs, at
// once; for an exit that skips the closes.
func EndServers() {
	var wg sync.WaitGroup
	liveServers.Range(func(_, f any) bool {
		wg.Add(1)
		go func() { defer wg.Done(); f.(func())() }()
		return true
	})
	wg.Wait()
}

// trackServer keeps stop as cmd's cleanup until it has run once.
func trackServer(cmd *exec.Cmd, stop func()) {
	var once sync.Once
	liveServers.Store(cmd, func() {
		once.Do(func() {
			stop()
			liveServers.Delete(cmd)
		})
	})
}

// ServerCommand runs a stdio MCP server in a container of its own, its stdio
// relayed by the engine: the session's mounts with read_only_paths in the home
// left out, its own home mounted as its HOME, its install folder in the home
// read-only, and nothing else of the home. Its network is open but under the
// allowlist, where it reaches only a proxy of its own whose decisions go to
// record. The program must run in the sandbox image.
func (c *Container) ServerCommand(ctx context.Context, name string, argv, env []string, record func(string, map[string]any) error) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, errors.New("sandbox: no command")
	}
	if ok, why := c.Available(); !ok {
		return nil, errors.New(why)
	}
	files, err := serverFilesIn(name, argv[0], env, PathForms(c.policy.Workspace))
	if err != nil {
		return nil, err
	}
	p := c.policy
	p.ReadOnlyPaths = files.outsideHome(p.ReadOnlyPaths)
	// Before this server's start: what a killed Abhed left goes first.
	sweptNote(c.SweepServers(ctx))
	extra := append(serverOwnerArgs(), "-v", files.home+":"+files.home)
	if files.prefix != "" {
		extra = append(extra, "-v", files.prefix+":"+files.prefix+":ro")
	}
	if err := c.programInImage(ctx, p, extra, argv[0]); err != nil {
		return nil, err
	}
	env = containerServerEnv(env, files.home)
	key := ServerKey(name)
	ctx = WithLaunch(ctx, Launch{CallID: key, Session: key, Record: record})
	var cmd *exec.Cmd
	var stop func()
	if c.policy.Egress != nil {
		cmd, stop = c.egressRun(ctx, c.policy.Workspace, serverPrefix, p, extra, env, argv)
	} else {
		cmd, stop = c.serverRun(ctx, p, extra, env, argv)
	}
	if cmd.Err != nil {
		return nil, fmt.Errorf("sandbox: the server was not started: %w", cmd.Err)
	}
	trackServer(cmd, stop)
	return cmd, nil
}

// serverRun is a server's container outside the allowlist, its network open,
// and what removes it.
func (c *Container) serverRun(ctx context.Context, p Policy, extra, env, argv []string) (*exec.Cmd, func()) {
	args, err := c.runArgsWith(ctx, p.Workspace, p, true)
	if err != nil {
		return &exec.Cmd{Err: err}, nil
	}
	// In a private file: a value may be a token, which an argument would show.
	envFile, removeEnv, err := EnvFile(env)
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("its environment: %w", err)}, nil
	}
	context.AfterFunc(ctx, removeEnv)
	name := containerName(serverPrefix)
	args = append(append(args, extra...), "--env-file", envFile, "--name", name, SandboxImage())
	cmd := c.engineCmd(ctx, append(args, argv...)...)
	cmd.Cancel = c.remove(cmd, name)
	return cmd, func() {
		c.removeNow(name)
		removeEnv()
	}
}

// programInImage refuses a server whose program the sandbox image cannot run:
// a macOS binary, or one the image does not have (npx, uvx, docker), looked
// up in a container with the server's mounts. An engine that fails the look
// is left to fail the start itself.
func (c *Container) programInImage(ctx context.Context, p Policy, extra []string, program string) error {
	missing := func(why string) error {
		return fmt.Errorf("sandbox: the server's program %s %s, so it is not started; set ABHED_SANDBOX_IMAGE to an image that has it, "+
			"or \"host_files\": true on the server to run it on the host", program, why)
	}
	if hostOnlyBinary(program, p.Workspace, mountedFrom(extra, p)) {
		return missing("is a macOS program, which the " + string(c.Tier()) + " tier's Linux image cannot run")
	}
	args, err := c.runArgsWith(ctx, p.Workspace, p, false)
	if err != nil {
		return err
	}
	args = append(append(args, extra...), "--name", containerName(serverPrefix+"look-"), SandboxImage(),
		"/bin/sh", "-c", lookProgram, "sh", program)
	cmd := c.engineCmd(ctx, args...)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == lookMissing {
			return missing("is not in the sandbox image " + SandboxImage())
		}
	}
	return nil
}

// lookProgram exits lookMissing when the image has no program $1; the
// engine's own failures are 125 to 127.
const (
	lookProgram = `if command -v "$1" >/dev/null 2>&1; then exit 0; fi; exit 3`
	lookMissing = 3
)

// mountedFrom are the host folders the server's container shows at their own
// paths: the workspace, read_only_paths, and those extra mounts.
func mountedFrom(extra []string, p Policy) []string {
	out := append([]string{p.Workspace}, p.ReadOnlyPaths...)
	for i := 0; i+1 < len(extra); i++ {
		if extra[i] == "-v" {
			host, _, _ := strings.Cut(extra[i+1], ":")
			out = append(out, host)
		}
	}
	return formsOf(out)
}

// hostOnlyBinary reports a program the container would run from a host
// folder it mounts (a path, never a bare name, which the image's PATH finds)
// that is a Mach-O binary: no Linux container runs it.
func hostOnlyBinary(program, workspace string, mounted []string) bool {
	if !strings.ContainsRune(program, '/') {
		return false
	}
	path := program
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path) // the container's working folder
	}
	if !slices.ContainsFunc(PathForms(path), func(q string) bool { return insideAny(q, mounted) }) {
		return false // the image's own file at that path, which the look below checks
	}
	f, err := os.Open(path) // #nosec G304 -- the configured server's program, read for its first bytes
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 4)
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	switch binary.BigEndian.Uint32(head) {
	case 0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe, 0xcafebabe:
		return true
	}
	return false
}

// containerServerEnv is a server's environment inside a container: HOME its
// own, and the host's PATH and TMPDIR left to the image's.
func containerServerEnv(env []string, home string) []string {
	hostPath := "PATH=" + os.Getenv("PATH")
	var out []string
	for _, kv := range serverEnv(env) {
		if kv == hostPath || strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "TMPDIR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home)
}
