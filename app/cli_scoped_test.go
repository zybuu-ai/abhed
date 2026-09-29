package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// cliSSHServer answers password logins and exec requests, and counts the
// connections still open.
func cliSSHServer(t *testing.T) (addr string, open *atomic.Int32) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if string(pw) == "vm-pw" {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	open = &atomic.Int32{}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				open.Add(1)
				defer open.Add(-1)
				go ssh.DiscardRequests(reqs)
				go func() {
					for nch := range chans {
						ch, in, err := nch.Accept()
						if err != nil {
							continue
						}
						go func() {
							for req := range in {
								_ = req.Reply(req.Type == "exec", nil)
								if req.Type == "exec" {
									_, _ = io.WriteString(ch, "abhed-connected\n")
									_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ S uint32 }{0}))
									_ = ch.Close()
								}
							}
						}()
					}
				}()
				_ = conn.Wait()
			}()
		}
	}()
	addr = ln.Addr().String()
	// Pin the host key where ssh_connect trusts one from.
	home := t.TempDir()
	t.Setenv("HOME", home)
	host, port, _ := net.SplitHostPort(addr)
	line := fmt.Sprintf("[%s]:%s %s", host, port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))))
	_ = os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return addr, open
}

// /clear and /resume start another conversation on the same tool session. A
// cluster login or SSH host from the last one must not carry into it: both
// are closed, and neither is reachable afterwards.
func TestNewConversationDropsLoginsAndHosts(t *testing.T) {
	var k8sOpen atomic.Int32
	cluster := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/version") {
			fmt.Fprint(w, `{"major":"1","minor":"29"}`)
			return
		}
		fmt.Fprint(w, `{"kind":"NodeList","items":[]}`)
	}))
	cluster.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			k8sOpen.Add(1)
		case http.StateClosed, http.StateHijacked:
			k8sOpen.Add(-1)
		}
	}
	cluster.StartTLS()
	defer cluster.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.Certificate().Raw}), 0o600)

	sshAddr, sshOpen := cliSSHServer(t)
	secret := func(name string) (string, error) {
		return map[string]string{"OCP_TOKEN": "tok-cli-1", "VM_PASSWORD": "vm-pw"}[name], nil
	}
	mgr := k8s.NewManager(k8s.Config{Kubeconfig: filepath.Join(t.TempDir(), "none"),
		Clusters: []k8s.LoginCluster{{Name: "prod", Server: cluster.URL, CAFile: ca}}})
	login, get := k8s.LoginTool{M: mgr, Secret: secret}, k8s.GetTool{M: mgr}
	connect, run := remote.ConnectTool{Secret: secret}, remote.Tool{}

	st, _, _, sess := resumeRig(t, "me", "default")
	ctx := context.Background()
	getProd := json.RawMessage(`{"resource":"nodes","cluster":"prod"}`)
	onVM := json.RawMessage(`{"host":"vm1","command":"id"}`)
	connectOnce := func(t *testing.T) {
		t.Helper()
		if res := login.Run(ctx, sess, json.RawMessage(`{"cluster":"prod","token_secret":"OCP_TOKEN"}`)); res.IsError {
			t.Fatalf("login: %s", res.Content)
		}
		if res := get.Run(ctx, sess, getProd); res.IsError {
			t.Fatalf("get: %s", res.Content)
		}
		args, _ := json.Marshal(map[string]string{"addr": sshAddr, "user": "tester", "name": "vm1", "password_secret": "VM_PASSWORD"})
		if res := connect.Run(ctx, sess, args); res.IsError {
			t.Fatalf("connect: %s", res.Content)
		}
		if res := run.Run(ctx, sess, onVM); res.IsError {
			t.Fatalf("ssh: %s", res.Content)
		}
	}
	gone := func(t *testing.T, after string) {
		t.Helper()
		if res := get.Run(ctx, sess, getProd); !res.IsError || !strings.Contains(res.Content, "not logged in") {
			t.Fatalf("after %s the last conversation's login still works: %s", after, res.Content)
		}
		if res := run.Run(ctx, sess, onVM); !res.IsError {
			t.Fatalf("after %s the last conversation's host still runs commands: %s", after, res.Content)
		}
		for deadline := time.Now().Add(3 * time.Second); k8sOpen.Load() != 0 || sshOpen.Load() != 0; {
			if time.Now().After(deadline) {
				t.Fatalf("after %s: %d cluster and %d ssh connections still open", after, k8sOpen.Load(), sshOpen.Load())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	for _, cmd := range []string{"/clear", "/resume s-old"} {
		connectOnce(t)
		handleCommand(ctx, cmd, ui.NewRenderer(io.Discard, false), policy.New(policy.ModeDefault), sess, st)
		gone(t, cmd)
	}
}
