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
	"crypto/ed25519"
	"crypto/fips140"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zybuu-ai/abhed/internal/egress"
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
	// connected marks a host ssh_connect added, whose errors name its
	// arguments rather than config keys.
	connected bool
	// machine marks a host read from the person's ~/.ssh/config: it signs in
	// with their ssh-agent and the IdentityFiles it names that need no
	// passphrase, checks every known_hosts file it names, and reaches the
	// host through jump when ProxyJump names one.
	machine        bool
	identityFiles  []string
	knownHostFiles []string
	jump           *Host
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

	if h.cfg.machine {
		methods = append(methods, h.machineKeys()...)
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
		if h.cfg.machine {
			return nil, fmt.Errorf("host %s has no usable credentials: start your ssh-agent and add the key "+
				"(ssh-add), or name a key with no passphrase in IdentityFile", h.cfg.Name)
		}
		if h.cfg.connected {
			return nil, fmt.Errorf("host %s has no usable credentials: start an ssh-agent, "+
				"or give ssh_connect identity_file or password_secret", h.cfg.Name)
		}
		return nil, fmt.Errorf("host %s has no usable credentials: "+
			"start an ssh-agent, set identity_file, or set password_env", h.cfg.Name)
	}
	return methods, nil
}

// hostKeyCallback verifies the server against known_hosts, and returns the
// host key algorithms to offer: those known_hosts holds for this host first,
// as OpenSSH orders them, or nil for the library's default order.
//
// Skipping verification is what makes SSH vulnerable to a machine-in-the-middle,
// and an agent connecting to a host it cannot authenticate will happily run
// commands on whatever answered.
func (h *Host) hostKeyCallback() (ssh.HostKeyCallback, []string, error) {
	if h.cfg.InsecureSkipHostKeyCheck {
		return ssh.InsecureIgnoreHostKey(), nil, nil
	}
	if len(h.cfg.knownHostFiles) > 0 {
		var files []string
		for _, f := range h.cfg.knownHostFiles {
			if _, err := os.Stat(expandHome(f)); err == nil {
				files = append(files, expandHome(f))
			}
		}
		if len(files) == 0 {
			return nil, nil, fmt.Errorf("host %s: none of its known_hosts files (%s) exists, so its key cannot be "+
				"checked. Ask the user to run `%s` once to record the key", h.cfg.Name,
				strings.Join(h.cfg.knownHostFiles, ", "), sshCommand(h.cfg.User, h.cfg.Addr))
		}
		cb, err := knownhosts.New(files...)
		if err != nil {
			return nil, nil, fmt.Errorf("host %s: read %s: %w", h.cfg.Name, strings.Join(files, ", "), err)
		}
		return cb, knownAlgorithms(cb, h.cfg.Addr), nil
	}
	path := h.cfg.KnownHostsFile
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("no known_hosts configured and no home directory")
		}
		path = filepath.Join(home, ".ssh", "known_hosts")
	}
	path = expandHome(path)
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, nil, fmt.Errorf("host %s: read %s: %w. "+
			"Connect once with ssh to record the host key, or set "+
			"insecure_skip_host_key_check for a throwaway VM", h.cfg.Name, path, err)
	}
	return cb, knownAlgorithms(cb, h.cfg.Addr), nil
}

// probeKey is a key no known_hosts holds, asked of the callback to learn
// which keys it does hold for a host.
var probeKey = sync.OnceValue(func() ssh.PublicKey {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil
	}
	return k
})

// knownAlgorithms is the host key algorithm list with the types known_hosts
// holds for addr first, then the rest of the library's list; nil when it
// holds none. Without it a server with several keys may show one not on
// file (the library prefers ecdsa to ed25519) and be refused, where OpenSSH,
// asking for the type it knows, connects. Only the order changes: the
// callback still accepts nothing but a key on file.
func knownAlgorithms(cb ssh.HostKeyCallback, addr string) []string {
	probe := probeKey()
	_, port, err := net.SplitHostPort(addr)
	if probe == nil || err != nil {
		return nil
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if err := cb(addr, &net.TCPAddr{IP: net.IPv4zero, Port: p}, probe); !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		return nil
	}
	// An insecure algorithm (ssh-rsa's SHA-1, ssh-dss) is offered only for a
	// key of that type on file, and never in FIPS mode, as the library does.
	all := ssh.SupportedAlgorithms().HostKeys
	insecure := ssh.InsecureAlgorithms().HostKeys
	var first []string
	for _, k := range keyErr.Want {
		algos := []string{k.Key.Type()}
		if algos[0] == ssh.KeyAlgoRSA {
			algos = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
		}
		for _, a := range algos {
			known := slices.Contains(all, a) || slices.Contains(insecure, a) && !fips140.Enabled()
			if known && !slices.Contains(first, a) {
				first = append(first, a)
			}
		}
	}
	if len(first) == 0 {
		return nil
	}
	rest := slices.DeleteFunc(slices.Clone(all), func(a string) bool { return slices.Contains(first, a) })
	return append(first, rest...)
}

// sshCommand is the ssh command line that reaches addr as user, with -p for
// a port other than 22: user@host:port is not ssh syntax.
func sshCommand(user, addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "ssh " + user + "@" + addr
	}
	if port == "22" {
		return "ssh " + user + "@" + host
	}
	return "ssh -p " + port + " " + user + "@" + host
}

// knownHostsName is addr as known_hosts and ssh-keygen -R name it.
func knownHostsName(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "22" {
		return strings.TrimSuffix(addr, ":22")
	}
	return "[" + host + "]:" + port
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
	hostKey, algorithms, err := h.hostKeyCallback()
	if err != nil {
		return nil, err
	}

	// Judged by the egress allowlist when one is in force, as a command's
	// ssh through the proxy would be.
	dctx, cancel := ctx, context.CancelFunc(func() {})
	if h.cfg.Timeout > 0 {
		dctx, cancel = context.WithTimeout(ctx, h.cfg.Timeout)
	}
	var conn net.Conn
	if h.cfg.jump != nil {
		// Through the jump host, whose own key is checked when it connects.
		var jc *ssh.Client
		if jc, err = h.cfg.jump.connect(dctx); err == nil {
			conn, err = jc.DialContext(dctx, "tcp", h.cfg.Addr)
		}
		if err != nil {
			cancel()
			return nil, fmt.Errorf("cannot reach %s (%s) through %s: %w", h.cfg.Name, h.cfg.Addr, h.cfg.jump.Name(), err)
		}
	} else {
		conn, err = egress.Dial(dctx, egress.KindSSH, "tcp", h.cfg.Addr)
	}
	cancel()
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s (%s): %w", h.cfg.Name, h.cfg.Addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, h.cfg.Addr, &ssh.ClientConfig{
		User:              h.cfg.User,
		Auth:              methods,
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: algorithms,
		Timeout:           h.cfg.Timeout,
	})
	if err != nil {
		_ = conn.Close()
		record := sshCommand(h.cfg.User, h.cfg.Addr)
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
			// A key of a type on file that is not the one on file: the
			// machine was rebuilt, or something is in the middle.
			return nil, fmt.Errorf("the host key %s offered does not match the one known_hosts "+
				"holds for it, so Abhed will not connect: the machine may have been rebuilt, or "+
				"something may be in the middle. Ask the user to confirm the new key out of band; "+
				"if it is right, they can remove the old one with `ssh-keygen -R '%s'` and run `%s` "+
				"once to record the new one: %w",
				h.cfg.Name, knownHostsName(h.cfg.Addr), record, err)
		}
		if (strings.Contains(err.Error(), "knownhosts") ||
			strings.Contains(err.Error(), "key is unknown")) && h.cfg.password != "" {
			return nil, fmt.Errorf("the host key for %s is not in known_hosts, and a stored "+
				"password is sent only to a host whose key is. Ask the user to run `%s` "+
				"once to record the key, or to connect with a key file: %w",
				h.cfg.Name, record, err)
		}
		if strings.Contains(err.Error(), "knownhosts") ||
			strings.Contains(err.Error(), "key is unknown") {
			retry := ""
			if h.cfg.connected {
				// accept_host_key is ssh_connect's; a declared host has none.
				retry = "If the user has said this host is new or ephemeral, retry with " +
					"accept_host_key: true. Otherwise ask "
			} else {
				retry = "Ask "
			}
			return nil, fmt.Errorf("the host key for %s is not in known_hosts, so Abhed "+
				"cannot confirm this is the machine you meant. %sthe user to run `%s` "+
				"once to record the key: %w",
				h.cfg.Name, retry, record, err)
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
	return h.RunInput(ctx, command, timeout, nil)
}

// RunInput is Run with stdin given to the command; nil gives it none.
func (h *Host) RunInput(ctx context.Context, command string, timeout time.Duration, stdin []byte) (*Output, error) {
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
	if stdin != nil {
		session.Stdin = bytes.NewReader(stdin)
	}

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
	if h.cfg.jump != nil {
		_ = h.cfg.jump.Close()
	}
	if h.client != nil {
		err := h.client.Close()
		h.client = nil
		return err
	}
	return nil
}

// machineKeys are a ~/.ssh/config host's keys: the ssh-agent's, then each
// IdentityFile that needs no passphrase, read in Abhed's process only. One
// with a passphrase is left to the agent, as ssh-add puts it there.
func (h *Host) machineKeys() []ssh.AuthMethod {
	var out []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			out = append(out, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	var signers []ssh.Signer
	for _, f := range h.cfg.identityFiles {
		key, err := os.ReadFile(expandHome(f)) // #nosec G304 -- a key the person's own ssh_config names
		if err != nil {
			continue
		}
		if s, err := ssh.ParsePrivateKey(key); err == nil {
			signers = append(signers, s)
		}
	}
	if len(signers) > 0 {
		out = append(out, ssh.PublicKeys(signers...))
	}
	return out
}

// drop closes c, and forgets it when it is still h's connection, so the
// next call dials again; another connection h holds now is left alone.
func (h *Host) drop(c *ssh.Client) {
	if c == nil {
		return
	}
	h.mu.Lock()
	if h.client == c {
		h.client = nil
	}
	h.mu.Unlock()
	_ = c.Close()
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
