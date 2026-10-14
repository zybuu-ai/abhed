package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a buffer the proxy writes to while the test reads it.
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

func writeEgressConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "egress.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEgressProxyServesTheAllowlist(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "reached") }))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	_, port, _ := net.SplitHostPort(u.Host)
	cfg := writeEgressConfig(t, fmt.Sprintf(`{"rules":[{"host":"127.0.0.1","ports":[%s],"methods":["GET"],"decision":"allow","allow_ips":["127.0.0.1"]}]}`, port))
	pol, err := loadEgressPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	out, errOut := &lockedBuffer{}, &lockedBuffer{}
	done := make(chan error, 1)
	go func() { done <- serveEgressProxy(ctx, ln, pol, "sbx-1", out, errOut) }()

	proxy, _ := url.Parse("http://" + ln.Addr().String())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}, Timeout: 10 * time.Second}
	resp, err := client.Get(up.URL + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "reached" {
		t.Fatalf("allowed request = %d %q", resp.StatusCode, body)
	}
	// Not in the allowlist: refused, whatever credentials are sent.
	resp, err = client.Post(up.URL+"/no", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a POST no rule allows = %d", resp.StatusCode)
	}
	if resp, err := client.Get("http://192.0.2.1/"); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("an unlisted host = %d", resp.StatusCode)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var allow, deny int
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec struct {
			Event   string         `json:"event"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Event != "egress.decision" || rec.Payload["call_id"] != "sbx-1" {
			t.Fatalf("record line %q (%v)", line, err)
		}
		switch rec.Payload["decision"] {
		case "allow":
			allow++
		case "deny":
			deny++
		}
	}
	if allow != 1 || deny < 2 {
		t.Fatalf("recorded %d allowed and %d denied:\n%s", allow, deny, out.String())
	}
	// Closed with the context: nothing serves the port any more.
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("the listener outlived the proxy")
	}
}

func TestEgressProxyRefusesWhatItCannotHonour(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":   `{"rules":[],"default_rule":"allow"}`,
		"repeated key":  `{"default":"deny","default":"allow"}`,
		"inspect_tls":   `{"inspect_tls":true}`,
		"bad decision":  `{"rules":[{"host":"a.test","decision":"maybe"}]}`,
		"two documents": `{} {}`,
	} {
		if _, err := loadEgressPolicy(writeEgressConfig(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var out, errOut bytes.Buffer
	if code := egressProxyCmd([]string{"-listen", "127.0.0.1:0"}, &out, &errOut); code != 2 {
		t.Errorf("no -config = %d", code)
	}
	ok := writeEgressConfig(t, `{"rules":[{"host":"*.example.com","decision":"allow"}]}`)
	if code := egressProxyCmd([]string{"-config", ok, "-check"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "1 rules, default deny") {
		t.Errorf("-check = %d %q %q", code, out.String(), errOut.String())
	}
	bad := writeEgressConfig(t, `{"inspect_tls":true}`)
	if code := egressProxyCmd([]string{"-config", bad, "-listen", "127.0.0.1:0"}, &out, &errOut); code != 1 {
		t.Errorf("an unsupported config = %d", code)
	}
}

// The proxy takes no credential, so a default of allow on an address other
// than loopback is an open proxy: refused unless -open says something in front decides.
func TestEgressProxyRefusesAnOpenProxy(t *testing.T) {
	allow := writeEgressConfig(t, `{"default":"allow","rules":[{"host":"metadata.internal.test","decision":"deny"}]}`)
	for _, addr := range []string{":8851", "0.0.0.0:8851", "192.0.2.1:8851"} {
		var out, errOut bytes.Buffer
		if code := egressProxyCmd([]string{"-config", allow, "-listen", addr}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "open proxy") {
			t.Errorf("%s: %d %q", addr, code, errOut.String())
		}
	}
	for addr, want := range map[string]bool{"127.0.0.1:8851": true, "[::1]:8851": true, "localhost:8851": true, ":8851": false, "10.0.0.1:8851": false} {
		if got := loopbackListen(addr); got != want {
			t.Errorf("loopbackListen(%s) = %v", addr, got)
		}
	}
}

// failingListener stops accepting at once, as a listener whose socket died.
type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) {
	return nil, fmt.Errorf("accept: too many open files")
}

// A proxy whose listener stops accepting exits with an error, so whatever
// runs it restarts it, rather than serving nothing until it is signalled.
func TestEgressProxyExitsWhenTheListenerDies(t *testing.T) {
	pol, err := loadEgressPolicy(writeEgressConfig(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		done <- serveEgressProxy(context.Background(), failingListener{ln}, pol, "c", io.Discard, io.Discard)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped accepting") {
			t.Fatalf("returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy kept running with no listener")
	}
}
