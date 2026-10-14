package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// An open workbench tab holds a shared stream and an idle session's event
// stream, neither of which ends by itself: a shutdown ends them rather than
// waiting out its whole budget for them.
func TestShutdownEndsOpenStreams(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{})})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("X-Abhed-User", "ana")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == ""; time.Sleep(20 * time.Millisecond) {
		resp, err := http.Post(base+"/v1/health", "", nil)
		if err == nil {
			resp.Body.Close()
			r := do("POST", "/v1/sessions", `{"workbench":true}`)
			var c createResponse
			_ = json.NewDecoder(r.Body).Decode(&c)
			r.Body.Close()
			if c.SessionID == "" {
				t.Fatalf("create: %d", r.StatusCode)
			}
			id = c.SessionID
		} else if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}

	events := do("GET", "/v1/sessions/"+id+"/events", "")
	defer events.Body.Close()
	shared := do("GET", "/v1/streams", "")
	defer shared.Body.Close()
	line, _ := bufio.NewReader(shared.Body).ReadString('\n')
	if events.StatusCode != http.StatusOK || shared.StatusCode != http.StatusOK || !strings.HasPrefix(line, "event: mux") {
		t.Fatalf("streams: %d %d %q", events.StatusCode, shared.StatusCode, line)
	}

	// Both stay open until the shutdown.
	ended := make(chan time.Time, 2)
	for _, b := range []*http.Response{events, shared} {
		go func() {
			buf := make([]byte, 512)
			for {
				if _, err := b.Body.Read(buf); err != nil {
					ended <- time.Now()
					return
				}
			}
		}()
	}
	select {
	case <-ended:
		t.Fatal("a stream ended before the shutdown")
	case <-time.After(300 * time.Millisecond):
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the shutdown waited %s on open streams", took.Round(time.Millisecond))
	}
	for range 2 {
		if at := <-ended; at.Before(start) {
			t.Fatal("a stream ended before the shutdown")
		}
	}
}

// heldRouter is a store that records which process holds each session.
type heldRouter struct {
	EventStore
	mu   sync.Mutex
	node map[string]string
}

func (h *heldRouter) ClaimNode(_ context.Context, id, node string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.node[id] = node
	return nil
}

func (h *heldRouter) ReleaseNode(_ context.Context, id, node string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.node[id] == node {
		delete(h.node, id)
	}
	return nil
}

func (h *heldRouter) NodeFor(_ context.Context, id string, _ time.Duration) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.node[id], nil
}

// A stop with a workbench tab open lets go of every session this process
// still holds, so another replica can take them at once rather than once the
// claim is stale.
func TestShutdownReleasesEveryClaim(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "proxy"
	st := &heldRouter{EventStore: agent.NewMemStore(), node: map[string]string{}}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{}, Store: st, NodeID: "node-a",
		Registry: tools.NewRegistry(tools.Read{})})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == ""; time.Sleep(20 * time.Millisecond) {
		req, _ := http.NewRequest("POST", base+"/v1/sessions", strings.NewReader(`{"workbench":true}`))
		req.Header.Set("X-Abhed-User", "ana")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			continue
		}
		var c createResponse
		_ = json.NewDecoder(resp.Body).Decode(&c)
		resp.Body.Close()
		id = c.SessionID
	}
	req, _ := http.NewRequest("GET", base+"/v1/sessions/"+id+"/events", nil)
	req.Header.Set("X-Abhed-User", "ana")
	events, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Body.Close()
	// Held by this process, as the tab's work left it when the drain began.
	s.mu.RLock()
	live := s.running[id]
	s.mu.RUnlock()
	if live == nil {
		t.Fatal("the session is not live")
	}
	if err := s.holdNode(live); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.NodeFor(context.Background(), id, 0); n == "" {
		t.Fatal("the session is not held")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not stop")
	}
	if n, _ := st.NodeFor(context.Background(), id, 0); n != "" {
		t.Fatalf("the stopped process still holds the session: %q", n)
	}
}
