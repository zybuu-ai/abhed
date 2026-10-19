package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// fakeJudge serves chat completions whose content is reply.
func fakeJudge(t *testing.T, reply string, seen *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k-test" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if seen != nil {
			_ = json.Unmarshal(b, seen)
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": reply}}}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelJudgeReadsTheSchema(t *testing.T) {
	var body map[string]any
	srv := fakeJudge(t, `{"decision":"deny","code":"goal-hijack","confidence":0.9,"rationale":"the host came from a web page","advice":"ask the person"}`, &body)
	m := Model{Endpoint: srv.URL + "/v1", Model: "guard-1", APIKey: "k-test"}
	v, err := m.Judge(context.Background(), mutating(policy.Allow))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != policy.Deny || v.Code != "goal-hijack" || v.Advice != "ask the person" || v.Version != "model-v1/guard-1" {
		t.Fatalf("%+v", v)
	}
	if body["model"] != "guard-1" || body["temperature"].(float64) != 0 {
		t.Fatalf("request %v", body)
	}
	// The key goes in the header, never in what the judge reads.
	if b, _ := json.Marshal(body); strings.Contains(string(b), "k-test") {
		t.Fatal("the API key is in the prompt")
	}
}

// Anything but the fixed schema is no answer, and the guard fails closed.
func TestModelJudgeFailsClosed(t *testing.T) {
	for name, reply := range map[string]string{
		"prose":       "Looks fine to me, allow it.",
		"unknown":     `{"decision":"approve","code":"ok"}`,
		"extra field": `{"decision":"allow","code":"ok","override":true}`,
		"two":         `{"decision":"allow"} {"decision":"allow"}`,
	} {
		srv := fakeJudge(t, reply, nil)
		g := &Guard{Monitor: Model{Endpoint: srv.URL + "/v1", Model: "m", APIKey: "k-test"}}
		out := g.Review(context.Background(), mutating(policy.Allow))
		if !out.Unavailable || out.After != policy.Ask {
			t.Errorf("%s: %+v", name, out)
		}
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	g := &Guard{Monitor: Model{Endpoint: slow.URL, Model: "m"}, Timeout: 50 * time.Millisecond, OnUnavailable: policy.Deny}
	if out := g.Review(context.Background(), mutating(policy.Allow)); !out.Unavailable || out.After != policy.Deny {
		t.Fatalf("timeout: %+v", out)
	}
	down := &Guard{Monitor: Model{Endpoint: "http://127.0.0.1:1", Model: "m"}}
	if out := down.Review(context.Background(), mutating(policy.Allow)); !out.Unavailable || out.After != policy.Ask {
		t.Fatalf("down: %+v", out)
	}
}

// A persuaded model cannot loosen: its allow leaves an ask an ask, and the
// rules' deny is never even put to it.
func TestChainOnlyTightens(t *testing.T) {
	asked := 0
	model := Func(func(context.Context, Case) (Verdict, error) {
		asked++
		return Verdict{Decision: policy.Allow, Code: "ok", Version: "m"}, nil
	})
	g := &Guard{Monitor: Chain{fixed(policy.Deny), model}}
	if out := g.Review(context.Background(), mutating(policy.Allow)); out.After != policy.Deny || asked != 0 {
		t.Fatalf("%+v asked=%d", out, asked)
	}
	g = &Guard{Monitor: Chain{fixed(policy.Ask), model}}
	if out := g.Review(context.Background(), mutating(policy.Allow)); out.After != policy.Ask || asked != 1 {
		t.Fatalf("%+v asked=%d", out, asked)
	}
	failing := Func(func(context.Context, Case) (Verdict, error) { return Verdict{}, fmt.Errorf("down") })
	g = &Guard{Monitor: Chain{fixed(policy.Ask), failing}, OnUnavailable: policy.Deny}
	if out := g.Review(context.Background(), mutating(policy.Allow)); out.After != policy.Deny || !out.Unavailable {
		t.Fatalf("%+v", out)
	}
}

// Every decision a verdict can name, from every source, against every
// provisional decision: the result is never looser than what policy said.
func TestNoVerdictLoosens(t *testing.T) {
	decisions := []policy.Decision{policy.Allow, policy.Ask, policy.Deny, "", "maybe"}
	for _, before := range []policy.Decision{policy.Allow, policy.Ask, policy.Deny} {
		for _, d := range decisions {
			for _, unattended := range []bool{false, true} {
				for _, onUn := range []policy.Decision{"", policy.Ask, policy.Deny, policy.Allow} {
					g := &Guard{Monitor: Chain{fixed(d), fixed(policy.Allow)}, Unattended: unattended, OnUnavailable: onUn}
					out := g.Review(context.Background(), mutating(before))
					if rank[out.After] < rank[before] {
						t.Fatalf("before %s verdict %q unattended %v: loosened to %s", before, d, unattended, out.After)
					}
				}
			}
		}
	}
}
