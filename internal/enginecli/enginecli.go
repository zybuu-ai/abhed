// Package enginecli runs a container engine's CLI with the environment it
// needs. On a Mac or Windows, Podman's CLI reaches its machine over ssh unless
// told of the machine's API socket; Podman 5.2 and earlier leave a root ssh
// session in the VM behind at each call, so a podman machine's own socket is
// used where it reaches the same engine as the user's default connection.
package enginecli

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// goos is the system the rules apply to; tests set it.
var goos = runtime.GOOS

// Guard, when set, is asked before an engine's CLI runs and an error refuses
// it. Tests use it to keep off the host's real engine.
var Guard func(path string) error

// askWait bounds each local query of the engine's configuration.
const askWait = 3 * time.Second

var (
	mu      sync.Mutex
	sockets = map[string]*machine{}
)

// machine is what one engine path's configuration said, asked once.
type machine struct {
	once sync.Once
	sock string
}

// Reset forgets what was learned of every engine. For tests.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	sockets = map[string]*machine{}
}

// Command is the engine at path with args, in the environment Env gives it.
func Command(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- the configured container engine
	if Guard != nil {
		if err := Guard(path); err != nil {
			cmd.Err = err
			return cmd
		}
	}
	cmd.Env = Env(path)
	return cmd
}

// Env is the host's environment for the engine at path, with CONTAINER_HOST
// set to a podman machine's API socket when neither CONTAINER_HOST nor
// CONTAINER_CONNECTION names an engine, the socket exists, and the default
// connection is that machine's root one.
func Env(path string) []string {
	env := os.Environ()
	if sock := machineSocket(path); sock != "" {
		env = append(env, "CONTAINER_HOST=unix://"+sock)
	}
	return env
}

func machineSocket(path string) string {
	if goos != "darwin" && goos != "windows" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(filepath.Base(path)), "podman") {
		return ""
	}
	// DOCKER_HOST is not read by podman, so it chooses nothing here; shells
	// often set it for docker-compatible tools.
	for _, k := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION"} {
		if os.Getenv(k) != "" {
			return ""
		}
	}
	mu.Lock()
	m := sockets[path]
	if m == nil {
		m = &machine{}
		sockets[path] = m
	}
	mu.Unlock()
	m.once.Do(func() { m.sock = askSocket(path) })
	if m.sock == "" {
		return ""
	}
	if info, err := os.Stat(m.sock); err != nil || info.Mode()&os.ModeSocket == 0 {
		return ""
	}
	return m.sock
}

// askSocket asks the CLI, from its local configuration alone, for the API
// socket of the machine whose root connection is the default; "" for any other.
func askSocket(path string) string {
	out, err := ask(path, "system", "connection", "list", "--format", "json")
	if err != nil {
		return ""
	}
	var conns []struct {
		Name      string
		URI       string
		IsMachine bool
		Default   bool
	}
	if json.Unmarshal(out, &conns) != nil {
		return ""
	}
	for _, c := range conns {
		if !c.Default {
			continue
		}
		u, err := url.Parse(c.URI)
		name, root := strings.CutSuffix(c.Name, "-root")
		if !c.IsMachine || !root || err != nil || u.Scheme != "ssh" || u.User.Username() != "root" {
			return ""
		}
		// The machine must be rootful, so its socket is root's as the connection is.
		out, err := ask(path, "machine", "inspect", "--format", "{{.Rootful}} {{.ConnectionInfo.PodmanSocket.Path}}", name)
		if err != nil {
			return ""
		}
		rootful, sock, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		if rootful != "true" || !filepath.IsAbs(sock) {
			return ""
		}
		return sock
	}
	return ""
}

func ask(path string, args ...string) ([]byte, error) {
	if Guard != nil {
		if err := Guard(path); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), askWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- the configured container engine, fixed arguments
	cmd.WaitDelay = time.Second
	return cmd.Output()
}
