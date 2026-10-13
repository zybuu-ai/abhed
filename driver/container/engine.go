package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/zybuu-ai/abhed/internal/enginecli"
)

// runner calls the engine's CLI. Tests replace it to check the exact argv.
type runner interface {
	// output runs the engine to completion and returns its stdout; a failure
	// is an *engineError carrying its stderr and exit code.
	output(ctx context.Context, args []string, stdin io.Reader) ([]byte, error)
	// command is the engine with args, for a caller that streams.
	command(ctx context.Context, args []string) *exec.Cmd
}

// engineError is a failed call to the engine's CLI.
type engineError struct {
	args   []string
	code   int
	stderr string
	err    error
}

func (e *engineError) Error() string {
	verb := ""
	if len(e.args) > 0 {
		verb = e.args[0]
	}
	msg := strings.TrimSpace(e.stderr)
	if msg == "" && e.err != nil {
		msg = e.err.Error()
	}
	return fmt.Sprintf("%s %s: %s", "engine", verb, msg)
}

func (e *engineError) Unwrap() error { return e.err }

// notFound reports an engine error saying the container does not exist.
func notFound(err error) bool {
	var ee *engineError
	if !errors.As(err, &ee) {
		return false
	}
	s := strings.ToLower(ee.stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no container with name or id")
}

// nameInUse reports an engine error saying the name is taken.
func nameInUse(err error) bool {
	var ee *engineError
	return errors.As(err, &ee) && strings.Contains(strings.ToLower(ee.stderr), "already in use")
}

// cli runs the engine binary found at path.
type cli struct{ path string }

func (c cli) command(ctx context.Context, args []string) *exec.Cmd {
	// The CLI gets the host's environment; only -e names reach the container.
	return enginecli.Command(ctx, c.path, args...)
}

func (c cli) output(ctx context.Context, args []string, stdin io.Reader) ([]byte, error) {
	cmd := c.command(ctx, args)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &stdout, &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return stdout.Bytes(), &engineError{args: args, code: code, stderr: stderr.String(), err: err}
	}
	return stdout.Bytes(), nil
}

// engineInfo is what the probe learned about the engine.
type engineInfo struct {
	// podman is true for Podman under any name, such as the podman-docker
	// wrapper installed as docker.
	podman   bool
	version  string
	rootless bool
	selinux  bool
	// remote is an engine reached over the network or in a VM, where a
	// host Unix socket cannot be bound into a container.
	remote bool
}

func (i engineInfo) backend(name string) string {
	flavor := "docker"
	if i.podman {
		flavor = "podman"
	}
	s := flavor + " " + i.version
	if flavor != name {
		s += " (as " + name + ")"
	}
	if i.rootless {
		s += ", rootless"
	}
	if i.selinux {
		s += ", SELinux"
	}
	return s
}

// probe asks the engine what it is; an engine that does not answer is
// unavailable.
func probe(ctx context.Context, r runner) (engineInfo, error) {
	out, err := r.output(ctx, []string{"--version"}, nil)
	if err != nil {
		return engineInfo{}, err
	}
	var info engineInfo
	info.podman = strings.Contains(strings.ToLower(string(out)), "podman")
	if info.podman {
		out, err = r.output(ctx, []string{"info", "--format", "{{.Version.Version}} {{.Host.Security.Rootless}} {{.Host.Security.SELinuxEnabled}}"}, nil)
		if err != nil {
			return engineInfo{}, err
		}
		f := strings.Fields(string(out))
		if len(f) < 3 {
			return engineInfo{}, fmt.Errorf("podman info answered %q", strings.TrimSpace(string(out)))
		}
		info.version, info.rootless, info.selinux = f[0], f[1] == "true", f[2] == "true"
	} else {
		out, err = r.output(ctx, []string{"info", "--format", "{{.ServerVersion}} {{json .SecurityOptions}}"}, nil)
		if err != nil {
			return engineInfo{}, err
		}
		s := strings.TrimSpace(string(out))
		v, opts, _ := strings.Cut(s, " ")
		if v == "" {
			return engineInfo{}, fmt.Errorf("docker info answered %q", s)
		}
		info.version = v
		info.rootless = strings.Contains(opts, "name=rootless")
		info.selinux = strings.Contains(opts, "name=selinux")
	}
	info.remote = remoteEngine(info.podman)
	return info, nil
}

// remoteEngine reports an engine this process reaches over a network or in
// a VM: an engine host that is not a local Unix socket, or Podman on a host
// where it always runs in a VM.
func remoteEngine(podman bool) bool {
	if hostOS != "linux" {
		return true
	}
	for _, k := range []string{"DOCKER_HOST", "CONTAINER_HOST"} {
		if v := os.Getenv(k); v != "" && !strings.HasPrefix(v, "unix://") {
			return true
		}
	}
	return podman && os.Getenv("CONTAINER_CONNECTION") != ""
}
