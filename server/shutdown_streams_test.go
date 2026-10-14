package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
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
