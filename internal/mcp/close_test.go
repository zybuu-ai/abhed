package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A server whose connect is still going when the gateway closes is closed
// too, not kept as a connection that outlives the gateway.
func TestCloseStopsAnInFlightConnect(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := `{"protocolVersion":"2024-11-05","serverInfo":{"name":"t","version":"1"},"capabilities":{"tools":{}}}`
		switch req.Method {
		case "initialize":
			close(started)
			<-release
		case "tools/list":
			result = `{"tools":[{"name":"x","inputSchema":{"type":"object"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	}))
	defer srv.Close()

	g := NewGateway()
	done := make(chan []error)
	go func() {
		done <- g.Connect(context.Background(), []ServerConfig{{Name: "slow", URL: srv.URL + "/mcp", Enabled: true}})
	}()
	<-started
	g.Close()
	close(release)
	errs := <-done
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "closed while the server connected") {
		t.Fatalf("errors %v", errs)
	}
	g.mu.RLock()
	n := len(g.clients)
	g.mu.RUnlock()
	if n != 0 || len(g.Tools()) != 0 {
		t.Fatalf("a connection finished after Close was kept: %d clients", n)
	}
}
