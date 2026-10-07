package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// TestMain lets this binary act as a stdio MCP server whose one tool
// returns the proxy URL, credential included, it was started with.
func TestMain(m *testing.M) {
	if os.Getenv("ABHED_MCP_FAKE_SERVER") == "1" {
		fakeServer()
		return
	}
	os.Exit(m.Run())
}

func fakeServer() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "fake", "version": "1"},
				"capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "proxy", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": os.Getenv("HTTP_PROXY")}}}
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		fmt.Println(string(b))
	}
}

// A confined stdio server's credential is refused, 407, once the server is
// closed or restarted; the restarted server gets one of its own.
func TestConfinedServerCredentialEndsWithTheServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := int(netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://")).Port())
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "127.0.0.1", Ports: []int{port}, Decision: "allow", AllowIPs: []string{"127.0.0.1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	p := sandbox.DefaultPolicy(t.TempDir())
	p.Egress = pol
	sb := sandbox.NewProcess(p)
	if ok, why := sb.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	defer func() { _ = sb.Close() }()
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		t.Skip("stdio servers are refused as root under bubblewrap")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g := mcp.NewGateway()
	defer g.Close()
	g.MustConfine, g.Confine = true, sb.ServerCommand
	if errs := g.Connect(ctx, []mcp.ServerConfig{{Name: "fake", Command: exe, Env: []string{"ABHED_MCP_FAKE_SERVER=1"}, Enabled: true}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	credential := func() string {
		t.Helper()
		ts := g.Tools()
		if len(ts) != 1 {
			t.Fatalf("%d tools", len(ts))
		}
		r := ts[0].Run(ctx, nil, []byte(`{}`))
		if r.IsError || !strings.HasPrefix(r.Content, "http://") {
			t.Fatalf("the server's proxy: %+v", r)
		}
		return r.Content
	}
	status := func(proxy string) int {
		t.Helper()
		u, _ := url.Parse(proxy)
		c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true}, Timeout: 10 * time.Second}
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	first := credential()
	if s := status(first); s != http.StatusOK {
		t.Fatalf("the running server's credential: %d", s)
	}
	if err := g.Restart(ctx, "fake"); err != nil {
		t.Fatal(err)
	}
	if s := status(first); s != http.StatusProxyAuthRequired {
		t.Fatalf("the credential of a restarted server's old process: %d", s)
	}
	second := credential()
	if second == first || status(second) != http.StatusOK {
		t.Fatal("the restarted server has no credential of its own")
	}
	g.Close()
	if s := status(second); s != http.StatusProxyAuthRequired {
		t.Fatalf("the credential of a closed server: %d", s)
	}
}
