package app

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/config"
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
	if again != e || e.Port == 0 {
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
