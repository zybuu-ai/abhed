package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/profiles"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/wsconn"
)

// connect --stdio carries the bytes both ways over the relay, with the
// profile's token, and says in the server's words why it was refused.
func TestConnectStdio(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"sandbox sbx_0123456789abcdef"}}`))
			return
		}
		if r.URL.Path != "/v1/sandboxes/sbx_0123456789abcdef:ssh" {
			http.Error(w, "path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		ws, err := wsconn.Upgrade(w, r, wsconn.UpgradeOptions{Protocols: []string{server.SSHRelayProtocol}})
		if err != nil {
			return
		}
		s := wsconn.NewStream(ws)
		buf := make([]byte, 64)
		n, _ := s.Read(buf)
		_, _ = s.Write(bytes.ToUpper(buf[:n]))
		_ = ws.Close()
	}))
	defer srv.Close()
	var out bytes.Buffer
	p := &profiles.Profile{URL: srv.URL, Token: "tok"}
	if err := connectStdio(context.Background(), p, "sbx_0123456789abcdef", strings.NewReader("ssh-2.0"), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "SSH-2.0" {
		t.Fatalf("relayed %q", out.String())
	}
	p.Token = "wrong"
	err := connectStdio(context.Background(), p, "sbx_0123456789abcdef", strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "(404): sandbox sbx_0123456789abcdef") {
		t.Fatalf("refusal: %v", err)
	}
	var he *wsconn.HandshakeError
	if !errors.As(err, &he) && !strings.Contains(err.Error(), "refused") {
		t.Fatal(err)
	}
}

func TestSandboxCmdUsage(t *testing.T) {
	var errOut bytes.Buffer
	for _, args := range [][]string{nil, {"connect", "sbx_0123456789abcdef"}, {"list"}} {
		errOut.Reset()
		if code := sandboxCmd(t.TempDir(), args, strings.NewReader(""), &bytes.Buffer{}, &errOut); code != 2 || !strings.Contains(errOut.String(), "usage") {
			t.Errorf("%v: %d %q", args, code, errOut.String())
		}
	}
	if code := sandboxCmd(t.TempDir(), []string{"connect", "--stdio", "not-an-id"}, strings.NewReader(""), &bytes.Buffer{}, &errOut); code != 1 {
		t.Fatalf("a bad id: %d", code)
	}
}
