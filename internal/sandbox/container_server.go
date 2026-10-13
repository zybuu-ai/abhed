package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// serverPrefix names a server's container.
var serverPrefix = "abhed-mcp-"

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
	extra := []string{"-v", files.home + ":" + files.home}
	if files.prefix != "" {
		extra = append(extra, "-v", files.prefix+":"+files.prefix+":ro")
	}
	env = containerServerEnv(env, files.home)
	key := ServerKey(name)
	ctx = WithLaunch(ctx, Launch{CallID: key, Session: key, Record: record})
	var cmd *exec.Cmd
	if c.policy.Egress != nil {
		cmd = c.egressRun(ctx, c.policy.Workspace, p, extra, env, argv)
	} else {
		cmd = c.serverRun(ctx, p, extra, env, argv)
	}
	if cmd.Err != nil {
		return nil, fmt.Errorf("sandbox: the server was not started: %w", cmd.Err)
	}
	return cmd, nil
}

// serverRun is a server's container outside the allowlist, its network open.
func (c *Container) serverRun(ctx context.Context, p Policy, extra, env, argv []string) *exec.Cmd {
	args, err := c.runArgsWith(ctx, p.Workspace, p, true)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	// In a private file: a value may be a token, which an argument would show.
	envFile, removeEnv, err := EnvFile(env)
	if err != nil {
		return &exec.Cmd{Err: fmt.Errorf("its environment: %w", err)}
	}
	context.AfterFunc(ctx, removeEnv)
	name := containerName(serverPrefix)
	args = append(append(args, extra...), "--env-file", envFile, "--name", name, Image)
	cmd := c.engineCmd(ctx, append(args, argv...)...)
	cmd.Cancel = c.remove(cmd, name)
	return cmd
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
