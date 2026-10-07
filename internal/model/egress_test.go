package model

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Under the allowlist the model's endpoint is reached and recorded by the
// rule "model", loopback included; any other host from its client is judged.
func TestModelEndpointAllowedUnderTheAllowlist(t *testing.T) {
	srv := sseServer(t,
		`{"choices":[{"delta":{"content":"ok"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	defer srv.Close()
	pol, err := egress.Compile(egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	g := egress.NewGuard(egress.GuardOptions{Policy: pol})
	defer g.Close()
	defer egress.Install(g)()

	var mu sync.Mutex
	var got []map[string]any
	ctx := egress.WithCaller(context.Background(), egress.Caller{Session: "s1",
		Record: func(_ string, m map[string]any) error { mu.Lock(); got = append(got, m); mu.Unlock(); return nil }})
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	ch, err := c.Complete(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for chunk := range ch {
		if chunk.Type == ChunkError {
			t.Fatal(chunk.Err)
		}
		text.WriteString(chunk.Text)
	}
	if text.String() != "ok" {
		t.Fatalf("got %q", text.String())
	}
	// Another host from the same client is judged: no rule allows it.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://other.test/", nil)
	var de *egress.DeniedError
	if _, err := c.HTTP.Do(req); !errors.As(err, &de) {
		t.Fatalf("the model client reached another host: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0]["rule"] != egress.RuleModel || got[0]["decision"] != "allow" ||
		got[0]["kind"] != egress.KindModel || got[0]["session"] != "s1" || got[1]["decision"] != "deny" {
		t.Fatalf("records: %v", got)
	}
}

// A policy that could not be loaded still lets the model through.
func TestModelEndpointAllowedWithoutAPolicy(t *testing.T) {
	srv := sseServer(t, `{"choices":[{"delta":{"content":"ok"}}]}`)
	defer srv.Close()
	g := egress.NewGuard(egress.GuardOptions{Err: errors.New("broken rules")})
	defer g.Close()
	defer egress.Install(g)()
	c := NewOpenAICompatible(srv.URL, "", "test", Profile{})
	text, _, _, _, errs := collect(t, c)
	if len(errs) > 0 || text != "ok" {
		t.Fatalf("%q %v", text, errs)
	}
}
