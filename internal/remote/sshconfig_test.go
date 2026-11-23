package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// machineHome is a throwaway home with an ~/.ssh folder; the real one is never read.
func machineHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("USER", "localme")
	if err := os.MkdirAll(filepath.Join(home, ".ssh", "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	grants := filepath.Join(home, ".abhed", "state", "machine-grants.json")
	old := machinegrant.Path
	machinegrant.Path = func() string { return grants }
	t.Cleanup(func() { machinegrant.Path = old })
	return home
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSSHConfigReadsAsOpenSSHDoes(t *testing.T) {
	home := machineHome(t)
	writeFile(t, filepath.Join(home, ".ssh", "conf.d", "lab"), "Host labbox\n  HostName 10.1.1.1\n  User labber\n")
	writeFile(t, filepath.Join(home, ".ssh", "config"), `# defaults first are overridden by nothing later
Include conf.d/*

Host prod
  HostName 10.0.0.5
  User admin
  Port 2222
  IdentityFile ~/.ssh/id_prod
  ProxyJump bastion

Host bastion
  HostName b.example.com
  User jumper

Host *.lab !skip.lab
  User wild

Host proxied
  ProxyCommand nc %h %p

Match host prod
  User fromMatch

Host *
  User fallback
  IdentityFile ~/.ssh/id_%r
  Port 2200
`)
	c, err := LoadSSHConfig(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Aliases(); !slices.Equal(got, []string{"bastion", "labbox", "prod", "proxied"}) {
		t.Fatalf("aliases %v", got)
	}
	prod, ok := c.Resolve("prod")
	if !ok {
		t.Fatal("prod not found")
	}
	// The first value wins; IdentityFile adds; Match applies to nothing.
	if prod.HostName != "10.0.0.5" || prod.User != "admin" || prod.Port != 2222 || prod.ProxyJump != "bastion" {
		t.Fatalf("prod = %+v", prod)
	}
	wantKeys := []string{filepath.Join(home, ".ssh", "id_prod"), filepath.Join(home, ".ssh", "id_admin")}
	if !slices.Equal(prod.IdentityFiles, wantKeys) {
		t.Fatalf("prod keys %v, want %v", prod.IdentityFiles, wantKeys)
	}
	if lab, _ := c.Resolve("labbox"); lab.HostName != "10.1.1.1" || lab.User != "labber" || lab.Port != 2200 {
		t.Fatalf("included labbox = %+v", lab)
	}
	if px, _ := c.Resolve("proxied"); !strings.Contains(px.Refused, "ProxyCommand") {
		t.Fatalf("a ProxyCommand host was not refused: %+v", px)
	}
	// Patterns and negation are honoured for a name no Host line gives.
	if h := c.resolve("db.lab"); h.User != "wild" {
		t.Fatalf("db.lab user %q", h.User)
	}
	if h := c.resolve("skip.lab"); h.User != "fallback" {
		t.Fatalf("negated skip.lab user %q", h.User)
	}
	if _, ok := c.Resolve("db.lab"); ok {
		t.Fatal("a wildcard was offered as a host")
	}
	if len(c.Unsupported) != 1 || !strings.Contains(c.Unsupported[0], "Match") {
		t.Fatalf("unsupported %v", c.Unsupported)
	}
}

func TestMissingSSHConfigIsEmpty(t *testing.T) {
	home := machineHome(t)
	c, err := LoadSSHConfig(filepath.Join(home, ".ssh", "config"))
	if err != nil || len(c.Aliases()) != 0 {
		t.Fatalf("%v %v", c.Aliases(), err)
	}
}

// keyServer accepts one public key, runs exec requests, and forwards
// direct-tcpip channels, as a jump host does.
type keyServer struct {
	addr     string
	hostKey  ssh.PublicKey
	mu       sync.Mutex
	commands []string
	forwards []string
}

func startKeyServer(t *testing.T, allowed ssh.PublicKey) *keyServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(allowed.Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &keyServer{addr: ln.Addr().String(), hostKey: signer.PublicKey()}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, cfg)
		}
	}()
	return s
}

func (s *keyServer) serve(nConn net.Conn, cfg *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "direct-tcpip":
			p := nc.ExtraData()
			hl := binary.BigEndian.Uint32(p[:4])
			host, port := string(p[4:4+hl]), binary.BigEndian.Uint32(p[4+hl:8+hl])
			target := net.JoinHostPort(host, fmt.Sprint(port))
			s.mu.Lock()
			s.forwards = append(s.forwards, target)
			s.mu.Unlock()
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = nc.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, rq, err := nc.Accept()
			if err != nil {
				return
			}
			go ssh.DiscardRequests(rq)
			go func() { _, _ = io.Copy(ch, up); _ = ch.Close() }()
			go func() { _, _ = io.Copy(up, ch); _ = up.Close() }()
		case "session":
			ch, rq, err := nc.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = ch.Close() }()
				for req := range rq {
					if req.Type != "exec" {
						_ = req.Reply(false, nil)
						continue
					}
					var p struct{ Command string }
					_ = ssh.Unmarshal(req.Payload, &p)
					_ = req.Reply(true, nil)
					s.mu.Lock()
					s.commands = append(s.commands, p.Command)
					s.mu.Unlock()
					_, _ = fmt.Fprintf(ch, "ran: %s\n", p.Command)
					_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					return
				}
			}()
		default:
			_ = nc.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

// clientKey writes an unencrypted key to path and returns its public half.
func clientKey(t *testing.T, path string) ssh.PublicKey {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(pem.EncodeToMemory(block)))
	signer, _ := ssh.NewSignerFromKey(priv)
	return signer.PublicKey()
}

func knownLine(addr string, key ssh.PublicKey) string {
	host, port, _ := net.SplitHostPort(addr)
	return "[" + host + "]:" + port + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + "\n"
}

// machineRegistry offers the home's ~/.ssh/config.
func machineRegistry(t *testing.T, home string) *Registry {
	t.Helper()
	c, err := LoadSSHConfig(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewRegistry(nil)
	r.UseSSHConfig(c, nil)
	return r
}

func run(t *testing.T, r *Registry, sess *tools.Session, host string) tools.Result {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"host": host, "command": "uptime"})
	return Tool{R: r}.Run(context.Background(), sess, args)
}

func TestMachineHostConnectsWithItsKeyAndKnownHosts(t *testing.T) {
	home := machineHome(t)
	pub := clientKey(t, filepath.Join(home, ".ssh", "id_lab"))
	srv := startKeyServer(t, pub)
	host, port, _ := net.SplitHostPort(srv.addr)
	writeFile(t, filepath.Join(home, ".ssh", "config"), fmt.Sprintf(
		"Host lab\n  HostName %s\n  Port %s\n  User deploy\n  IdentityFile ~/.ssh/id_lab\n", host, port))
	known := filepath.Join(home, ".ssh", "known_hosts")
	writeFile(t, known, knownLine(srv.addr, srv.hostKey))
	before, _ := os.ReadFile(known)

	r := machineRegistry(t, home)
	sess, _ := tools.NewSession(t.TempDir())
	defer sess.CloseScoped()
	res := run(t, r, sess, "lab")
	if res.IsError || !strings.Contains(res.Content, "ran: uptime") || !strings.Contains(res.Content, "deploy@lab") {
		t.Fatalf("ssh on a ~/.ssh/config host: %+v", res)
	}
	if !strings.Contains((Tool{R: r}).Description(), "lab") {
		t.Error("the description does not name the ~/.ssh/config host")
	}
	after, _ := os.ReadFile(known)
	if string(before) != string(after) {
		t.Fatal("known_hosts was written")
	}
}

func TestMachineHostWithUnknownKeyIsRefusedAndNothingWritten(t *testing.T) {
	home := machineHome(t)
	pub := clientKey(t, filepath.Join(home, ".ssh", "id_lab"))
	srv := startKeyServer(t, pub)
	host, port, _ := net.SplitHostPort(srv.addr)
	writeFile(t, filepath.Join(home, ".ssh", "config"), fmt.Sprintf(
		"Host lab\n  HostName %s\n  Port %s\n  IdentityFile ~/.ssh/id_lab\n", host, port))
	known := filepath.Join(home, ".ssh", "known_hosts")
	writeFile(t, known, "# nothing yet\n")

	r := machineRegistry(t, home)
	sess, _ := tools.NewSession(t.TempDir())
	defer sess.CloseScoped()
	res := run(t, r, sess, "lab")
	if !res.IsError || !strings.Contains(res.Content, "known_hosts") {
		t.Fatalf("an unknown host key was not refused: %+v", res)
	}
	if b, _ := os.ReadFile(known); string(b) != "# nothing yet\n" {
		t.Fatalf("known_hosts was written: %q", b)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.commands) != 0 {
		t.Fatal("a command ran on a host whose key was not checked")
	}
}

func TestMachineHostThroughProxyJump(t *testing.T) {
	home := machineHome(t)
	pub := clientKey(t, filepath.Join(home, ".ssh", "id_lab"))
	jump := startKeyServer(t, pub)
	target := startKeyServer(t, pub)
	jh, jp, _ := net.SplitHostPort(jump.addr)
	th, tp, _ := net.SplitHostPort(target.addr)
	writeFile(t, filepath.Join(home, ".ssh", "config"), fmt.Sprintf(`Host inner
  HostName %s
  Port %s
  ProxyJump bastion
Host bastion
  HostName %s
  Port %s
Host *
  IdentityFile ~/.ssh/id_lab
`, th, tp, jh, jp))
	writeFile(t, filepath.Join(home, ".ssh", "known_hosts"), knownLine(jump.addr, jump.hostKey)+knownLine(target.addr, target.hostKey))

	r := machineRegistry(t, home)
	sess, _ := tools.NewSession(t.TempDir())
	defer sess.CloseScoped()
	res := run(t, r, sess, "inner")
	if res.IsError || !strings.Contains(res.Content, "ran: uptime") {
		t.Fatalf("through ProxyJump: %+v", res)
	}
	jump.mu.Lock()
	defer jump.mu.Unlock()
	if len(jump.forwards) != 1 || jump.forwards[0] != target.addr || len(jump.commands) != 0 {
		t.Fatalf("the jump host forwarded %v and ran %v", jump.forwards, jump.commands)
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if len(target.commands) != 1 {
		t.Fatalf("the target ran %v", target.commands)
	}
}

func TestMachineHostFirstUseIsAskedOnceAndKept(t *testing.T) {
	home := machineHome(t)
	cfg := filepath.Join(home, ".ssh", "config")
	writeFile(t, cfg, "Host lab\n  HostName 10.9.9.9\n  User deploy\n")
	r := machineRegistry(t, home)
	sess, _ := tools.NewSession(t.TempDir())
	defer sess.CloseScoped()
	args := json.RawMessage(`{"host":"lab","command":"uptime"}`)
	tool := Tool{R: r}
	g, need := tool.FirstUse(sess, args)
	if !need || g.Kind != machinegrant.KindSSHHost || !strings.Contains(g.Reason, "deploy@10.9.9.9:22") {
		t.Fatalf("first use: %v %+v", need, g)
	}
	if !tools.IsFirstUseScope(g.Scope) {
		t.Fatalf("scope %q is not a first-use scope", g.Scope)
	}
	if err := tool.KeepFirstUse(g); err != nil {
		t.Fatal(err)
	}
	if _, need := tool.FirstUse(sess, args); need {
		t.Fatal("asked again after it was kept")
	}
	// Another machine behind the same name is asked about again.
	writeFile(t, cfg, "Host lab\n  HostName 10.9.9.10\n  User deploy\n")
	r2 := machineRegistry(t, home)
	if _, need := (Tool{R: r2}).FirstUse(sess, args); !need {
		t.Fatal("a changed HostName was not asked about")
	}
	// A declared host is the operator's: no first use, and its name wins.
	r3, _ := NewRegistry([]HostConfig{{Name: "lab", Addr: "10.0.0.1", User: "ops"}})
	c, _ := LoadSSHConfig(cfg)
	r3.UseSSHConfig(c, nil)
	if _, need := (Tool{R: r3}).FirstUse(sess, args); need {
		t.Fatal("a declared host asked a first use")
	}
	if _, ok := r3.MachineHost("lab"); ok {
		t.Fatal("a ~/.ssh/config host took a declared host's name")
	}
}

func TestConnectHostsBoundsMachineHosts(t *testing.T) {
	home := machineHome(t)
	writeFile(t, filepath.Join(home, ".ssh", "config"), "Host lab\n  HostName 10.9.9.9\n")
	c, _ := LoadSSHConfig(filepath.Join(home, ".ssh", "config"))
	r, _ := NewRegistry(nil)
	r.UseSSHConfig(c, []string{"*.example.com"})
	mh, ok := r.MachineHost("lab")
	if !ok || !strings.Contains(mh.Refused, "connect_hosts") {
		t.Fatalf("lab = %+v", mh)
	}
}

func TestConnectNamesAMachineHostReady(t *testing.T) {
	home := machineHome(t)
	writeFile(t, filepath.Join(home, ".ssh", "config"), "Host lab\n  HostName 10.9.9.9\n")
	r := machineRegistry(t, home)
	err := ConnectTool{R: r}.Precheck(nil, json.RawMessage(`{"addr":"lab"}`))
	if err == nil || !strings.Contains(err.Error(), "~/.ssh/config") {
		t.Fatalf("ssh_connect to a ~/.ssh/config host: %v", err)
	}
	// One that brings its own key goes ahead.
	if err := (ConnectTool{R: r}).Precheck(nil, json.RawMessage(`{"addr":"lab","identity_file":"/k"}`)); err != nil {
		t.Fatal(err)
	}
}
