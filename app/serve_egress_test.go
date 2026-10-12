package app

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver/microvm/guest"
	"github.com/zybuu-ai/abhed/internal/egress"
)

func TestSandboxEgressOnlyUnderTheAllowlist(t *testing.T) {
	cfg := config.Default()
	if eg, err := newSandboxEgress(cfg, nil); err != nil || eg != nil {
		t.Fatalf("allowlist off: %v %v", eg, err)
	}
}

func TestSandboxEgressServesTheAllowlistPerSandbox(t *testing.T) {
	cfg := config.Default()
	cfg.Sandbox.Network = config.NetworkAllowlist
	cfg.Egress.Rules = append(cfg.Egress.Rules, egress.Rule{Host: "allowed.example", Decision: "allow"})
	var mu sync.Mutex
	var recorded []map[string]any
	eg, err := newSandboxEgress(cfg, func(event string, p map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		recorded = append(recorded, p)
		return nil
	})
	if err != nil || eg == nil {
		t.Fatal(err)
	}
	if eg.For("process") || !eg.For("microvm") {
		t.Fatal("the proxy is for the microvm driver only")
	}
	const id = "sbx_0123456789abcdef"
	if _, _, err := eg.Call(id); err == nil {
		t.Fatal("a credential before the proxy was opened")
	}
	e, err := eg.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := eg.Open(id)
	if again.Socket != e.Socket || again.Port != e.Port || e.Port == 0 {
		t.Fatalf("open twice: %+v %+v", e, again)
	}
	other, _ := eg.Open("sbx_fedcba9876543210")
	if other.Socket == e.Socket {
		t.Fatal("two sandboxes share a proxy")
	}
	env, end, err := eg.Call(id)
	if err != nil {
		t.Fatal(err)
	}
	var proxyURL string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HTTP_PROXY="); ok {
			proxyURL = v
		}
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Port() != fmt.Sprint(e.Port) {
		t.Fatalf("proxy url %q for port %d", proxyURL, e.Port)
	}

	// Through the socket, with the call's credential, a destination the
	// allowlist does not name is refused and the refusal is recorded.
	c, err := net.Dial("unix", e.Socket)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := u.User.Password()
	auth := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pass))
	fmt.Fprintf(c, "GET http://denied.example/ HTTP/1.1\r\nHost: denied.example\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = c.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("an unlisted destination: %d", resp.StatusCode)
	}
	// The resolver socket takes the sandbox's DNS credential, which is not
	// the command's, and judges a lookup by the same rules.
	if e.DNSSocket == "" || e.DNSCredential == "" || strings.Contains(e.DNSCredential, auth) {
		t.Fatalf("dns %q %q", e.DNSSocket, e.DNSCredential)
	}
	if rcode := lookupRcode(t, e.DNSSocket, e.DNSCredential, "allowed.example"); rcode != 0 {
		t.Fatalf("a listed host: rcode %d", rcode)
	}
	if rcode := lookupRcode(t, e.DNSSocket, e.DNSCredential, "denied.example"); rcode != 3 {
		t.Fatalf("an unlisted host: rcode %d, want NXDOMAIN", rcode)
	}
	if rcode := lookupRcode(t, e.DNSSocket, "Basic d3Jvbmc6Y3JlZA==", "allowed.example"); rcode != 5 {
		t.Fatalf("a wrong credential: rcode %d, want REFUSED", rcode)
	}
	end()
	if err := eg.Close(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Socket); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
	eg.(*sandboxEgress).closeAll()
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, p := range recorded {
		found = found || p["sandbox_id"] == id && p["decision"] == "deny"
	}
	if !found {
		t.Fatalf("the refusal is not recorded with the sandbox: %v", recorded)
	}
}

// lookupRcode asks the resolver socket for name's A record and returns the
// answer's rcode.
func lookupRcode(t *testing.T, sock, cred, name string) int {
	t.Helper()
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range strings.Split(name, ".") {
		q = append(append(q, byte(len(l))), l...)
	}
	q = append(q, 0, 0, 1, 0, 1)
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	for _, b := range [][]byte{[]byte(cred), q} {
		if _, err := c.Write(append([]byte{byte(len(b) >> 8), byte(len(b))}, b...)); err != nil {
			t.Fatal(err)
		}
	}
	var n [2]byte
	if _, err := io.ReadFull(c, n[:]); err != nil {
		return -1 // refused without an answer
	}
	a := make([]byte, int(n[0])<<8|int(n[1]))
	if _, err := io.ReadFull(c, a); err != nil || len(a) < 4 {
		t.Fatalf("answer: %v", err)
	}
	return int(a[3] & 0x0f)
}

// Under inspect_tls a microVM sandbox's egress carries the session's CA
// bundle for the guest, and each call's clients are pointed at it there.
func TestSandboxEgressGivesTheMicroVMTheInspectionCA(t *testing.T) {
	cfg := config.Default()
	cfg.Sandbox.Network = config.NetworkAllowlist
	cfg.Egress.InspectTLS = true
	eg, err := newSandboxEgress(cfg, nil)
	if err != nil || eg == nil {
		t.Fatal(err)
	}
	const id = "sbx_0123456789abcdef"
	e, err := eg.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eg.Close(id) })
	if !strings.Contains(string(e.CABundle), "BEGIN CERTIFICATE") {
		t.Fatal("no CA bundle for the guest")
	}
	env, end, err := eg.Call(id)
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	if !slices.Contains(env, "SSL_CERT_FILE="+guest.CAFile) {
		t.Fatalf("the call is not pointed at the guest's bundle: %v", env)
	}
}
