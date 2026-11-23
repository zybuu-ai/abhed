package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
)

// A page on another site that points its own name at this server (DNS
// rebinding) sends that name as Host and as Origin; the guard refuses it.
// With no sign-in only loopback (and named hosts) are answered; with sign-in,
// IP addresses and configured names are too.
func TestHostGuardRefusesRebinding(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) })
	code := func(cfg config.Config, host string) int {
		req := httptest.NewRequest("POST", "/v1/sessions", nil)
		req.Host = host
		req.Header.Set("Origin", "http://"+host)
		rec := httptest.NewRecorder()
		hostGuard(cfg, nil, ok).ServeHTTP(rec, req)
		return rec.Code
	}
	none := config.Default()
	none.Auth.Mode = "none"
	named := none
	named.Server.AllowedHosts = []string{"Abhed.Lan"}
	signed := config.Default()
	signed.Auth.Mode = "local"
	signed.Server.AllowedOrigins = []string{"https://console.example.com"}
	signed.Server.CanonicalHost = "abhed.example.com"
	for _, c := range []struct {
		name string
		cfg  config.Config
		host string
		want int
	}{
		{"none, rebinding name", none, "rebind.evil.example:8853", http.StatusMisdirectedRequest},
		{"none, LAN address", none, "192.168.1.5:8853", http.StatusMisdirectedRequest},
		{"none, loopback v4", none, "127.0.0.1:8853", http.StatusAccepted},
		{"none, loopback v6", none, "[::1]:8853", http.StatusAccepted},
		{"none, localhost", none, "localhost:8853", http.StatusAccepted},
		{"none, localhost dot", none, "LOCALHOST.:8853", http.StatusAccepted},
		{"none, named", named, "abhed.lan:8853", http.StatusAccepted},
		{"none, localhost lookalike", none, "localhost.evil.example", http.StatusMisdirectedRequest},
		{"signed, rebinding name", signed, "rebind.evil.example", http.StatusMisdirectedRequest},
		{"signed, IP address", signed, "10.0.0.7:8443", http.StatusAccepted},
		{"signed, canonical", signed, "abhed.example.com", http.StatusAccepted},
		{"signed, origin's host", signed, "console.example.com", http.StatusAccepted},
	} {
		if got := code(c.cfg, c.host); got != c.want {
			t.Errorf("%s (%s): %d, want %d", c.name, c.host, got, c.want)
		}
	}
}

// On the network listener, as the eval's probe reached it: a rebinding Host
// and matching Origin are refused before the API; loopback is served.
func TestServeRefusesARebindingHost(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "none"
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	defer func() { cancel(); <-done }()
	addr := ln.Addr().String()
	send := func(host string) int {
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest("POST", "http://"+addr+"/v1/sessions", strings.NewReader(`{"prompt":"hi"}`))
			req.Host = host
			req.Header.Set("Origin", "http://"+host)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		t.Fatal("the server did not answer")
		return 0
	}
	if code := send("rebind.evil.example:8853"); code != http.StatusMisdirectedRequest {
		t.Fatalf("a rebinding host: %d", code)
	}
	if code := send(addr); code == http.StatusMisdirectedRequest {
		t.Fatalf("loopback refused: %d", code)
	}
}

// serve says, in one line, when it listens beyond loopback with no name
// for the host guard; loopback, or a name configured, says nothing.
func TestHostNamesNote(t *testing.T) {
	none := config.Default()
	none.Auth.Mode = "none"
	signed := config.Default()
	signed.Auth.Mode = "local"
	named := signed
	named.Server.AllowedHosts = []string{"abhed.lan"}
	for _, c := range []struct {
		cfg  config.Config
		addr string
		want bool
	}{
		{none, "127.0.0.1:8853", false}, {none, "localhost:8853", false}, {none, "[::1]:8853", false},
		{none, ":8853", true}, {signed, "0.0.0.0:8443", true}, {signed, "10.0.0.7:8443", true},
		{named, "0.0.0.0:8443", false},
	} {
		if got := HostNamesNote(c.cfg, c.addr) != ""; got != c.want {
			t.Errorf("%s (auth %q): note %v, want %v", c.addr, c.cfg.Auth.Mode, got, c.want)
		}
	}
}
