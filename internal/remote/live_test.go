package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// A real SSH server, so the transport, auth and host key verification are
// exercised rather than mocked. Everything below runs against it.
type testServer struct {
	addr     string
	hostKey  ssh.PublicKey
	stop     func()
	attempts *atomic.Int32 // passwords offered to it
}

func startSSHServer(t *testing.T, password string) *testServer {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	attempts := &atomic.Int32{}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			attempts.Add(1)
			if c.User() == "tester" && string(pass) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			go serveConn(conn, cfg)
		}
	}()

	return &testServer{
		addr: ln.Addr().String(), hostKey: signer.PublicKey(),
		stop:     func() { close(done); _ = ln.Close() },
		attempts: attempts,
	}
}

// serveConn answers exec requests with a canned result, which is all the tool
// needs to be exercised end to end.
func serveConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			return
		}
		go func(ch ssh.Channel, in <-chan *ssh.Request) {
			defer func() { _ = ch.Close() }()
			for req := range in {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &payload)
				_ = req.Reply(true, nil)

				status := 0
				switch {
				case strings.Contains(payload.Command, "false"):
					_, _ = fmt.Fprint(ch.Stderr(), "it failed\n")
					status = 3
				case strings.Contains(payload.Command, "hostname"):
					_, _ = fmt.Fprint(ch, "abhed-test-vm\n")
				default:
					_, _ = fmt.Fprintf(ch, "ran: %s\n", payload.Command)
				}
				_, _ = ch.SendRequest("exit-status", false,
					ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
				return
			}
		}(ch, requests)
	}
}

// knownHostsFor writes a known_hosts pinning the test server, so host key
// verification is genuinely exercised rather than skipped.
func knownHostsFor(t *testing.T, s *testServer) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "known_hosts")
	line := fmt.Sprintf("[%s]:%s %s\n",
		strings.Split(s.addr, ":")[0], strings.Split(s.addr, ":")[1],
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.hostKey))))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunAgainstRealSSHServer(t *testing.T) {
	srv := startSSHServer(t, "hunter2")
	defer srv.stop()

	t.Setenv("TEST_SSH_PW", "hunter2")
	reg, errs := NewRegistry([]HostConfig{{
		Name: "vm1", Addr: srv.addr, User: "tester",
		PasswordEnv:    "TEST_SSH_PW",
		KnownHostsFile: knownHostsFor(t, srv),
	}})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	defer reg.Close()

	args, _ := json.Marshal(map[string]any{"host": "vm1", "command": "hostname"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)

	if res.IsError {
		t.Fatalf("command failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "abhed-test-vm") {
		t.Errorf("stdout missing: %s", res.Content)
	}
	if !strings.Contains(res.Content, "tester@vm1") {
		t.Errorf("result does not say where it ran: %s", res.Content)
	}
}

// A non-zero exit is a result the model must see, not a transport failure.
func TestNonZeroExitIsReported(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	t.Setenv("TEST_SSH_PW", "pw")

	reg, _ := NewRegistry([]HostConfig{{
		Name: "vm1", Addr: srv.addr, User: "tester",
		PasswordEnv: "TEST_SSH_PW", KnownHostsFile: knownHostsFor(t, srv),
	}})
	defer reg.Close()

	args, _ := json.Marshal(map[string]any{"host": "vm1", "command": "false"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)

	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Errorf("exit code = %v, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Content, "it failed") {
		t.Errorf("stderr lost: %s", res.Content)
	}
}

// The point of host key verification: a server whose key is not pinned must
// be refused, not silently trusted.
func TestUnknownHostKeyIsRefused(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	t.Setenv("TEST_SSH_PW", "pw")

	empty := filepath.Join(t.TempDir(), "known_hosts")
	_ = os.WriteFile(empty, []byte(""), 0o600)

	reg, _ := NewRegistry([]HostConfig{{
		Name: "vm1", Addr: srv.addr, User: "tester",
		PasswordEnv: "TEST_SSH_PW", KnownHostsFile: empty,
	}})
	defer reg.Close()

	args, _ := json.Marshal(map[string]any{"host": "vm1", "command": "hostname"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)

	if !res.IsError {
		t.Fatal("connected to a host whose key was not pinned")
	}
	if !strings.Contains(res.Content, "known_hosts") {
		t.Errorf("refusal does not explain the fix: %s", res.Content)
	}
}

func TestWrongPasswordIsReported(t *testing.T) {
	srv := startSSHServer(t, "correct")
	defer srv.stop()
	t.Setenv("TEST_SSH_PW", "wrong")

	reg, _ := NewRegistry([]HostConfig{{
		Name: "vm1", Addr: srv.addr, User: "tester",
		PasswordEnv: "TEST_SSH_PW", KnownHostsFile: knownHostsFor(t, srv),
	}})
	defer reg.Close()

	args, _ := json.Marshal(map[string]any{"host": "vm1", "command": "hostname"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)

	if !res.IsError || !strings.Contains(res.Content, "authentication") {
		t.Errorf("auth failure unclear: %s", res.Content)
	}
}

// The connection is reused, so a multi-step task does not re-handshake per
// command.
func TestConnectionIsReused(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	t.Setenv("TEST_SSH_PW", "pw")

	h, err := NewHost(HostConfig{
		Name: "vm1", Addr: srv.addr, User: "tester",
		PasswordEnv: "TEST_SSH_PW", KnownHostsFile: knownHostsFor(t, srv),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	ctx := context.Background()
	if _, err := h.Run(ctx, "hostname", 5*time.Second); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := h.client
	if _, err := h.Run(ctx, "hostname", 5*time.Second); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if h.client != first {
		t.Error("reconnected instead of reusing the open connection")
	}
}

// A user pasting "key is at ~Downloads/key (1).prv" — missing slash, misspelled
// directory, a space in the name — should not send the agent hunting with
// glob through directories the sandbox denies.
func TestResolveKeyPathHandlesTypedPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dl := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Skip("cannot create Downloads")
	}
	name := "abhed-test-key (1).prv"
	real := filepath.Join(dl, name)
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Skip("cannot write test key")
	}
	defer func() { _ = os.Remove(real) }()

	for _, typed := range []string{
		real,                  // exact
		"~/Downloads/" + name, // tilde
		"~Downloads/" + name,  // missing slash AND misspelled, as reported
		name,                  // bare filename
	} {
		got, err := resolveKeyPath(typed)
		if err != nil {
			t.Errorf("resolveKeyPath(%q) failed: %v", typed, err)
			continue
		}
		if got != real {
			t.Errorf("resolveKeyPath(%q) = %q, want %q", typed, got, real)
		}
	}
}

// A path that genuinely does not exist must say where it looked, so the user
// can correct it rather than the agent guessing again.
func TestResolveKeyPathExplainsFailure(t *testing.T) {
	_, err := resolveKeyPath("~/nowhere/definitely-not-a-key-xyz.prv")
	if err == nil {
		t.Fatal("accepted a path that does not exist")
	}
	if !strings.Contains(err.Error(), "Tried:") {
		t.Errorf("error does not say where it looked: %v", err)
	}
}

// stored stands in for the secrets store.
func stored(vals map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		v, ok := vals[name]
		if !ok {
			return "", fmt.Errorf("no secret named %s is stored", name)
		}
		return v, nil
	}
}

// pinHome makes the test server's key the one in ~/.ssh/known_hosts, the
// only place ssh_connect trusts a host key from.
func pinHome(t *testing.T, s *testServer) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	data, err := os.ReadFile(knownHostsFor(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newSession(t *testing.T) *tools.Session {
	t.Helper()
	s, err := tools.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CloseScoped)
	return s
}

// ssh_connect must verify before registering: a host stored but unreachable
// turns one clear failure into a confusing one on the next command.
func TestConnectVerifiesBeforeRegistering(t *testing.T) {
	reg, _ := NewRegistry(nil)
	sess := newSession(t)
	args, _ := json.Marshal(map[string]any{
		"addr": "127.0.0.1:1", "user": "nobody", "accept_host_key": true})
	res := ConnectTool{R: reg}.Run(context.Background(), sess, args)

	if !res.IsError {
		t.Fatal("registered a host it could not reach")
	}
	if len(knownNames(reg, sessionHostsOf(sess, reg, false))) != 0 {
		t.Error("an unreachable host was registered anyway")
	}
}

func TestConnectRegistersWorkingHost(t *testing.T) {
	srv := startSSHServer(t, "pw-from-store")
	defer srv.stop()
	pinHome(t, srv)

	reg, _ := NewRegistry(nil)
	defer reg.Close()
	sess := newSession(t)

	host, port, _ := net.SplitHostPort(srv.addr)
	args, _ := json.Marshal(map[string]any{
		"addr": host + ":" + port, "user": "tester", "name": "vm1",
		"password_secret": "VM_PASSWORD"})
	connect := ConnectTool{R: reg, Secret: stored(map[string]string{"VM_PASSWORD": "pw-from-store"})}
	res := connect.Run(context.Background(), sess, args)

	if res.IsError {
		t.Fatalf("connect failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "not written to ~/.ssh/config") {
		t.Errorf("does not say where the credential lives: %s", res.Content)
	}
	if strings.Contains(res.Content, "pw-from-store") {
		t.Errorf("the result carries the password: %s", res.Content)
	}

	// And the ssh tool can now use it.
	runArgs, _ := json.Marshal(map[string]any{"host": "vm1", "command": "hostname"})
	run := Tool{R: reg}.Run(context.Background(), sess, runArgs)
	if run.IsError {
		t.Fatalf("registered host is not usable: %s", run.Content)
	}
}

// One registry serves every session on a server. A host one session connected
// must not be reachable, or even listed, from another.
func TestConnectedHostIsScopedToTheSession(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	pinHome(t, srv)
	reg, _ := NewRegistry(nil)
	defer reg.Close()
	a, b := newSession(t), newSession(t)

	args, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester", "name": "vm1",
		"password_secret": "VM_PASSWORD"})
	connect := ConnectTool{R: reg, Secret: stored(map[string]string{"VM_PASSWORD": "pw"})}
	if res := connect.Run(context.Background(), a, args); res.IsError {
		t.Fatalf("session A could not connect: %s", res.Content)
	}

	runArgs, _ := json.Marshal(map[string]any{"host": "vm1", "command": "hostname"})
	if res := (Tool{R: reg}).Run(context.Background(), a, runArgs); res.IsError {
		t.Fatalf("session A cannot use its own host: %s", res.Content)
	}
	for name, sess := range map[string]*tools.Session{"session B": b, "no session": nil} {
		res := Tool{R: reg}.Run(context.Background(), sess, runArgs)
		if !res.IsError || strings.Contains(res.Content, "abhed-test-vm") {
			t.Fatalf("%s ran a command on session A's host: %s", name, res.Content)
		}
		if strings.Contains(res.Content, "vm1") && !strings.Contains(res.Content, `"vm1"`) {
			t.Fatalf("%s is told about session A's host: %s", name, res.Content)
		}
	}
}

// With no session the host would be kept where every caller finds it, so
// ssh_connect refuses; nor may a session put its machine behind an operator's name.
func TestConnectRefusesWhatItCannotIsolate(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	pinHome(t, srv)
	reg, _ := NewRegistry([]HostConfig{{Name: "prod", Addr: "10.0.0.1", User: "ops"}})
	connect := ConnectTool{R: reg, Secret: stored(map[string]string{"VM_PASSWORD": "pw"})}

	args, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester", "name": "vm1",
		"password_secret": "VM_PASSWORD"})
	if res := connect.Run(context.Background(), nil, args); !res.IsError {
		t.Fatalf("connected with no session to hold the host: %s", res.Content)
	}
	shadow, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester", "name": "prod",
		"password_secret": "VM_PASSWORD"})
	if res := connect.Run(context.Background(), newSession(t), shadow); !res.IsError {
		t.Fatalf("a session replaced the operator's host: %s", res.Content)
	}
}

// A stored password goes only to a host whose key is already pinned: with
// accept_host_key, whoever answers at the address the model gave would get it.
func TestConnectSendsNoPasswordToAnUnpinnedHost(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	t.Setenv("HOME", t.TempDir()) // nothing pinned
	connect := ConnectTool{Secret: stored(map[string]string{"VM_PASSWORD": "pw"})}
	args, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester",
		"password_secret": "VM_PASSWORD", "accept_host_key": true})
	res := connect.Run(context.Background(), newSession(t), args)
	if !res.IsError || !strings.Contains(res.Content, "known_hosts") {
		t.Fatalf("a stored password was offered to an unpinned host: %s", res.Content)
	}
	if n := srv.attempts.Load(); n != 0 {
		t.Fatalf("%d passwords reached the host", n)
	}
}

// A password is named, never sent: the old password_env argument, which read
// any variable in Abhed's own environment, is gone, and a value put where the
// name belongs is withheld before anything records it.
func TestConnectTakesNoPasswordArgument(t *testing.T) {
	schema := string(ConnectTool{}.Schema())
	if strings.Contains(schema, "password_env") || strings.Contains(schema, `"password"`) {
		t.Fatalf("ssh_connect still takes a password or an environment variable: %s", schema)
	}
	canon, _, err := tools.CanonicalArgs(ConnectTool{}, json.RawMessage(
		`{"addr":"h","password":"hunter2-pasted","password_env":"OPENAI_API_KEY"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canon), "hunter2") || strings.Contains(string(canon), "OPENAI") {
		t.Fatalf("kept: %s", canon)
	}
	got := tools.WithholdSecretValues(ConnectTool{}, json.RawMessage(`{"addr":"h","password_secret":"hunter2 pasted"}`))
	if strings.Contains(string(got), "hunter2") {
		t.Fatalf("a password in password_secret was kept: %s", got)
	}
}

// Declaring a host changes which machines the agent can reach.
func TestConnectRequiresApproval(t *testing.T) {
	if !(ConnectTool{}).Mutates() {
		t.Error("ssh_connect does not declare itself mutating, so it could run unapproved")
	}
}

// The refusal must tell the model what to do, or it retries identically.
func TestUnknownHostKeyErrorNamesTheRetry(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	t.Setenv("TEST_SSH_PW", "pw")

	empty := filepath.Join(t.TempDir(), "known_hosts")
	_ = os.WriteFile(empty, []byte(""), 0o600)
	h, _ := NewHost(HostConfig{Name: "vm1", Addr: srv.addr, User: "tester",
		PasswordEnv: "TEST_SSH_PW", KnownHostsFile: empty})
	defer func() { _ = h.Close() }()

	_, err := h.Run(context.Background(), "hostname", 5*time.Second)
	if err == nil {
		t.Fatal("connected without a pinned host key")
	}
	if !strings.Contains(err.Error(), "accept_host_key") {
		t.Errorf("error does not name the retry option: %v", err)
	}
}

// When the session goes, the hosts it connected are closed, and a connect
// that finishes after that keeps nothing open.
func TestClosingTheSessionClosesItsHosts(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	pinHome(t, srv)
	sess := newSession(t)
	connect := ConnectTool{Secret: stored(map[string]string{"VM_PASSWORD": "pw"})}
	args, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester", "name": "vm1",
		"password_secret": "VM_PASSWORD"})
	if res := connect.Run(context.Background(), sess, args); res.IsError {
		t.Fatalf("connect failed: %s", res.Content)
	}
	h, ok := sessionHostsOf(sess, nil, false).get("vm1")
	if !ok {
		t.Fatal("host not kept")
	}
	sess.CloseScoped()
	h.mu.Lock()
	open := h.client != nil
	h.mu.Unlock()
	if open {
		t.Fatal("the session's host kept its connection after the session ended")
	}

	late, _ := NewHost(HostConfig{Name: "late", Addr: srv.addr, User: "tester"})
	if (&sessionHosts{hosts: map[string]*Host{}, closed: true}).add(late) {
		t.Fatal("a host added after the session ended was kept")
	}
	if res := connect.Run(context.Background(), sess, args); !res.IsError {
		t.Fatalf("a connect after the session ended registered a host: %s", res.Content)
	}
}

// A session's host cannot take a name that reads like an operator's, and the
// approval for a command names the account and address it runs on.
func TestSessionHostCannotPassForAnOperatorHost(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	pinHome(t, srv)
	reg, _ := NewRegistry([]HostConfig{{Name: "prod", Addr: "10.0.0.1", User: "ops"}})
	connect := ConnectTool{R: reg, Secret: stored(map[string]string{"VM_PASSWORD": "pw"})}
	for _, name := range []string{"Prod", "PROD", "prоd" /* Cyrillic о */} {
		args, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester", "name": name,
			"password_secret": "VM_PASSWORD"})
		if res := connect.Run(context.Background(), newSession(t), args); !res.IsError {
			t.Errorf("a session host named %q was allowed beside the operator's prod", name)
		}
	}

	sess := newSession(t)
	args, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester", "name": "vm1",
		"password_secret": "VM_PASSWORD"})
	if res := connect.Run(context.Background(), sess, args); res.IsError {
		t.Fatal(res.Content)
	}
	tool := Tool{R: reg}
	if got := tool.Target(sess, json.RawMessage(`{"host":"vm1","command":"id"}`)); !strings.Contains(got, "tester@"+srv.addr) {
		t.Errorf("the approval does not show where vm1 is: %q", got)
	}
	if got := tool.Target(sess, json.RawMessage(`{"host":"prod","command":"id"}`)); !strings.Contains(got, "ops@10.0.0.1:22") {
		t.Errorf("the approval does not show where prod is: %q", got)
	}
}

// A password connect to a host that is not pinned says how to pin it, not to
// accept any key, which ssh_connect refuses for a password.
func TestUnpinnedPasswordHostErrorNamesTheWayForward(t *testing.T) {
	srv := startSSHServer(t, "pw")
	defer srv.stop()
	t.Setenv("HOME", t.TempDir())
	_ = os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".ssh"), 0o700)
	_ = os.WriteFile(filepath.Join(os.Getenv("HOME"), ".ssh", "known_hosts"), nil, 0o600)
	connect := ConnectTool{Secret: stored(map[string]string{"VM_PASSWORD": "pw"})}
	args, _ := json.Marshal(map[string]any{"addr": srv.addr, "user": "tester", "password_secret": "VM_PASSWORD"})
	res := connect.Run(context.Background(), newSession(t), args)
	if !res.IsError || strings.Contains(res.Content, "accept_host_key: true") || !strings.Contains(res.Content, "key file") {
		t.Fatalf("the error does not name a way forward that works: %s", res.Content)
	}
	h, _ := NewHost(HostConfig{Name: "x", Addr: srv.addr, User: "tester", connected: true})
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := h.authMethods(); err == nil || strings.Contains(err.Error(), "password_env") {
		t.Fatalf("a session host is told to use a config key: %v", err)
	}
}
