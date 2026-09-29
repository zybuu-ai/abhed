// Package remote gives the agent access to machines over SSH.
//
// Abhed already runs commands locally through a sandbox. A deep agent working
// on real infrastructure also needs to reach the machines that infrastructure
// runs on — read a log on a VM, check a service, inspect a config.
//
// Every guarantee the local sandbox provides is absent here, and pretending
// otherwise would be the dangerous choice: a command on a remote host runs
// with that account's full authority, outside any workspace scoping, with no
// checkpoint and no /undo. So the design leans the other way — hosts are
// declared by the operator, the model can only name one of them, and running
// anything at all requires approval.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostConfig declares one reachable machine. Operator-supplied: the model
// selects a host by name and cannot introduce a new one.
type HostConfig struct {
	Name string `json:"name"`
	Addr string `json:"addr"` // host or host:port
	User string `json:"user"`

	// IdentityFile is a private key path. Empty uses the SSH agent, which is
	// the better default: the key never leaves the agent.
	IdentityFile string `json:"identity_file,omitempty"`
	// PasswordEnv names an environment variable holding a password, for hosts
	// that have no key. Never the password itself — a config file the agent
	// can read is the wrong place for one.
	PasswordEnv string `json:"password_env,omitempty"`

	// KnownHostsFile pins the host key. Defaults to ~/.ssh/known_hosts.
	KnownHostsFile string `json:"known_hosts_file,omitempty"`
	// InsecureSkipHostKeyCheck disables host key verification. It exists
	// because ephemeral lab VMs genuinely have no stable key, and refusing
	// would push people to run ssh through bash where Abhed sees nothing —
	// but it is off by default and reported at startup.
	InsecureSkipHostKeyCheck bool `json:"insecure_skip_host_key_check,omitempty"`

	Timeout time.Duration `json:"-"`

	// password is one ssh_connect read from the secrets store. Unexported so
	// no config file can carry it.
	password string
}

// Host is a connection to one machine, dialed lazily and reused across calls.
type Host struct {
	cfg HostConfig

	mu     sync.Mutex
	client *ssh.Client
}

func NewHost(cfg HostConfig) (*Host, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("ssh host needs a name")
	}
	if cfg.Addr == "" {
		return nil, fmt.Errorf("host %q needs an addr", cfg.Name)
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("host %q needs a user", cfg.Name)
	}
	if !strings.Contains(cfg.Addr, ":") {
		cfg.Addr += ":22"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Host{cfg: cfg}, nil
}

func (h *Host) Name() string { return h.cfg.Name }
func (h *Host) Addr() string { return h.cfg.Addr }
func (h *Host) User() string { return h.cfg.User }

// authMethods builds the credentials for this host, in the order that keeps
// secrets closest to where they live.
func (h *Host) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// The agent first: the key stays in the agent and Abhed never holds it.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" && h.cfg.IdentityFile == "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	if h.cfg.IdentityFile != "" {
		path := expandHome(h.cfg.IdentityFile)
		key, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("host %s: read identity file %s: %w", h.cfg.Name, path, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			// A passphrase-protected key cannot be used unattended. Say that,
			// rather than reporting a generic parse failure.
			if strings.Contains(err.Error(), "passphrase") {
				return nil, fmt.Errorf("host %s: %s is passphrase-protected; "+
					"add it to your ssh-agent instead (ssh-add %s)",
					h.cfg.Name, path, path)
			}
			return nil, fmt.Errorf("host %s: parse %s: %w", h.cfg.Name, path, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if h.cfg.password != "" {
		methods = append(methods, ssh.Password(h.cfg.password))
	}

	if h.cfg.PasswordEnv != "" {
		pw := os.Getenv(h.cfg.PasswordEnv)
		if pw == "" {
			return nil, fmt.Errorf("host %s: %s is not set in the environment",
				h.cfg.Name, h.cfg.PasswordEnv)
		}
		methods = append(methods, ssh.Password(pw))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("host %s has no usable credentials: "+
			"start an ssh-agent, set identity_file, or set password_env", h.cfg.Name)
	}
	return methods, nil
}

// hostKeyCallback verifies the server against known_hosts.
//
// Skipping this is what makes SSH vulnerable to a machine-in-the-middle, and
// an agent connecting to a host it cannot authenticate will happily run
// commands on whatever answered.
func (h *Host) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if h.cfg.InsecureSkipHostKeyCheck {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	path := h.cfg.KnownHostsFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("no known_hosts configured and no home directory")
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	path = expandHome(path)
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("host %s: read %s: %w. "+
			"Connect once with ssh to record the host key, or set "+
			"insecure_skip_host_key_check for a throwaway VM", h.cfg.Name, path, err)
	}
	return cb, nil
}

// connect dials the host, reusing an open connection.
func (h *Host) connect(ctx context.Context) (*ssh.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		// Cheap liveness check: a dead connection otherwise surfaces as a
		// confusing error on the next command.
		if _, _, err := h.client.SendRequest("keepalive@abhed", true, nil); err == nil {
			return h.client, nil
		}
		_ = h.client.Close()
		h.client = nil
	}

	methods, err := h.authMethods()
	if err != nil {
		return nil, err
	}
	hostKey, err := h.hostKeyCallback()
	if err != nil {
		return nil, err
	}

	dialer := net.Dialer{Timeout: h.cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", h.cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s (%s): %w", h.cfg.Name, h.cfg.Addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, h.cfg.Addr, &ssh.ClientConfig{
		User:            h.cfg.User,
		Auth:            methods,
		HostKeyCallback: hostKey,
		Timeout:         h.cfg.Timeout,
	})
	if err != nil {
		_ = conn.Close()
		if strings.Contains(err.Error(), "knownhosts") ||
			strings.Contains(err.Error(), "key is unknown") {
			return nil, fmt.Errorf("the host key for %s is not in known_hosts, so Abhed "+
				"cannot confirm this is the machine you meant. If the user has said this "+
				"host is new or ephemeral, retry with accept_host_key: true. Otherwise ask "+
				"them to run `ssh %s@%s` once to record the key: %w",
				h.cfg.Name, h.cfg.User, h.cfg.Addr, err)
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("authentication to %s@%s failed: %w",
				h.cfg.User, h.cfg.Name, err)
		}
		return nil, fmt.Errorf("ssh handshake with %s failed: %w", h.cfg.Name, err)
	}

	h.client = ssh.NewClient(sshConn, chans, reqs)
	return h.client, nil
}

// Output is the result of one remote command.
type Output struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes a command and waits for it.
func (h *Host) Run(ctx context.Context, command string, timeout time.Duration) (*Output, error) {
	client, err := h.connect(ctx)
	if err != nil {
		return nil, err
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session on %s: %w", h.cfg.Name, err)
	}
	defer func() { _ = session.Close() }()

	var stdout, stderr bytes.Buffer
	// Bounded: a command that prints a gigabyte must not take the agent down
	// with it, and the model cannot read that much anyway.
	session.Stdout = &limitedWriter{w: &stdout, limit: 1 << 20}
	session.Stderr = &limitedWriter{w: &stderr, limit: 256 << 10}

	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case err := <-done:
		out := &Output{Stdout: stdout.String(), Stderr: stderr.String()}
		if err != nil {
			var exitErr *ssh.ExitError
			if ok := asExitError(err, &exitErr); ok {
				out.ExitCode = exitErr.ExitStatus()
				return out, nil // a non-zero exit is a result, not a failure
			}
			return out, fmt.Errorf("command failed on %s: %w", h.cfg.Name, err)
		}
		return out, nil
	case <-runCtx.Done():
		// Signal the remote process; a session left running holds the
		// connection open and the next call inherits the mess.
		_ = session.Signal(ssh.SIGKILL) // best effort: the session is being abandoned either way
		return &Output{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: -1},
			fmt.Errorf("command timed out after %s on %s", timeout, h.cfg.Name)
	}
}

func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		err := h.client.Close()
		h.client = nil
		return err
	}
	return nil
}

func asExitError(err error, target **ssh.ExitError) bool {
	e := &ssh.ExitError{}
	if errors.As(err, &e) {
		*target = e
		return true
	}
	return false
}

type limitedWriter struct {
	w       *bytes.Buffer
	limit   int
	written int
	cut     bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.written >= l.limit {
		l.cut = true
		return len(p), nil // discard, but do not fail the command
	}
	room := l.limit - l.written
	if len(p) > room {
		l.w.Write(p[:room])
		l.written = l.limit
		l.cut = true
		return len(p), nil
	}
	l.w.Write(p)
	l.written += len(p)
	return len(p), nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
