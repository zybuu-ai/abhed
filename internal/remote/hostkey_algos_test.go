package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// multiKeyServer is startSSHServer's server with an ed25519, an ecdsa and an
// rsa host key, as a stock sshd has.
func multiKeyServer(t *testing.T, password string) (addr string, keys map[string]ssh.PublicKey) {
	t.Helper()
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
		if c.User() == "tester" && string(pass) == password {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	ecPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsaPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	keys = map[string]ssh.PublicKey{}
	for _, priv := range []any{edPriv, ecPriv, rsaPriv} {
		s, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AddHostKey(s)
		keys[s.PublicKey().Type()] = s.PublicKey()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg)
		}
	}()
	return ln.Addr().String(), keys
}

func knownHostsWith(t *testing.T, addr string, key ssh.PublicKey) string {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	path := filepath.Join(t.TempDir(), "known_hosts")
	line := fmt.Sprintf("[%s]:%s %s\n", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A known_hosts that holds one key type for a host, as ssh itself writes
// it, is enough: the client asks for the type on file, as OpenSSH does.
func TestOneKnownKeyTypeConnects(t *testing.T) {
	addr, keys := multiKeyServer(t, "pw")
	t.Setenv("TEST_SSH_PW", "pw")
	for typ, key := range keys {
		t.Run(typ, func(t *testing.T) {
			reg, errs := NewRegistry([]HostConfig{{
				Name: "app1", Addr: addr, User: "tester",
				PasswordEnv: "TEST_SSH_PW", KnownHostsFile: knownHostsWith(t, addr, key),
			}})
			if len(errs) > 0 {
				t.Fatal(errs)
			}
			defer reg.Close()
			args, _ := json.Marshal(map[string]any{"host": "app1", "command": "hostname"})
			if res := (Tool{R: reg}).Run(context.Background(), nil, args); res.IsError {
				t.Fatalf("refused a host whose %s key is on file: %s", typ, res.Content)
			}
		})
	}
}

// Ordering the algorithms changes nothing about what is accepted: a key of
// the known type that is not the one on file is still refused, as a
// mismatch, with no advice to accept it.
func TestKnownTypeWrongKeyRefused(t *testing.T) {
	addr, _ := multiKeyServer(t, "pw")
	t.Setenv("TEST_SSH_PW", "pw")
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	stranger, _ := ssh.NewPublicKey(other)
	reg, _ := NewRegistry([]HostConfig{{
		Name: "app1", Addr: addr, User: "tester",
		PasswordEnv: "TEST_SSH_PW", KnownHostsFile: knownHostsWith(t, addr, stranger),
	}})
	defer reg.Close()
	args, _ := json.Marshal(map[string]any{"host": "app1", "command": "hostname"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)
	if !res.IsError {
		t.Fatal("connected to a host whose key does not match known_hosts")
	}
	if !strings.Contains(res.Content, "does not match") || strings.Contains(res.Content, "accept_host_key") {
		t.Errorf("mismatch not explained as one: %s", res.Content)
	}
	_, port, _ := net.SplitHostPort(addr)
	if !strings.Contains(res.Content, "ssh -p "+port+" tester@127.0.0.1") {
		t.Errorf("no valid ssh command to record the key: %s", res.Content)
	}
}

// An unknown host is refused as before; a declared host is not told to use
// ssh_connect's accept_host_key, and the advice is a command ssh accepts.
func TestUnknownDeclaredHostAdvice(t *testing.T) {
	addr, _ := multiKeyServer(t, "pw")
	t.Setenv("TEST_SSH_PW", "pw")
	empty := filepath.Join(t.TempDir(), "known_hosts")
	_ = os.WriteFile(empty, nil, 0o600)
	reg, _ := NewRegistry([]HostConfig{{
		Name: "app1", Addr: addr, User: "tester", PasswordEnv: "TEST_SSH_PW", KnownHostsFile: empty,
	}})
	defer reg.Close()
	args, _ := json.Marshal(map[string]any{"host": "app1", "command": "hostname"})
	res := Tool{R: reg}.Run(context.Background(), nil, args)
	_, port, _ := net.SplitHostPort(addr)
	if !res.IsError || !strings.Contains(res.Content, "not in known_hosts") {
		t.Fatalf("unknown host not refused: %s", res.Content)
	}
	if strings.Contains(res.Content, "accept_host_key") || strings.Contains(res.Content, "@"+addr) ||
		!strings.Contains(res.Content, "`ssh -p "+port+" tester@127.0.0.1`") {
		t.Errorf("advice is not a valid ssh command for a declared host: %s", res.Content)
	}
}

func TestSSHCommandLine(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1:12222": "ssh -p 12222 root@127.0.0.1",
		"db.example:22":   "ssh root@db.example",
		"[::1]:2200":      "ssh -p 2200 root@::1",
	} {
		if got := sshCommand("root", addr); got != want {
			t.Errorf("%s: %q, want %q", addr, got, want)
		}
	}
	if got := knownHostsName("127.0.0.1:12222"); got != "[127.0.0.1]:12222" {
		t.Errorf("known_hosts name %q", got)
	}
	if got := knownHostsName("db.example:22"); got != "db.example" {
		t.Errorf("known_hosts name %q", got)
	}
}

// The offered list puts the known types first and adds an insecure one
// (ssh-rsa's SHA-1, ssh-dss) only for a key of that type on file.
func TestKnownAlgorithmsOrderAndInsecure(t *testing.T) {
	addr, keys := multiKeyServer(t, "pw")
	for typ, wantInsecure := range map[string]bool{ssh.KeyAlgoED25519: false, ssh.KeyAlgoRSA: true} {
		cb, err := knownhosts.New(knownHostsWith(t, addr, keys[typ]))
		if err != nil {
			t.Fatal(err)
		}
		got := knownAlgorithms(cb, addr)
		if len(got) == 0 {
			t.Fatalf("%s: no list", typ)
		}
		first := typ
		if typ == ssh.KeyAlgoRSA {
			first = ssh.KeyAlgoRSASHA512
		}
		if got[0] != first || slices.Contains(got, ssh.InsecureKeyAlgoDSA) || slices.Contains(got, ssh.KeyAlgoRSA) != wantInsecure {
			t.Errorf("%s: %v", typ, got)
		}
	}
}
