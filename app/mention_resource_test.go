package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/mcp"
)

// readLog keeps the URIs a server was asked to read.
type readLog struct {
	mu   sync.Mutex
	uris []string
}

func (r *readLog) add(u string) { r.mu.Lock(); r.uris = append(r.uris, u); r.mu.Unlock() }

func (r *readLog) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.uris...)
}

// resourceServer is an HTTP MCP server offering two resources, one of them
// holding a canary, and recording each read.
func resourceServer(t *testing.T, reads *readLog) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				URI string `json:"uri"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		result := `"result":{}`
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			result = `"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"resources":{}}}`
		case "tools/list":
			result = `"result":{"tools":[]}`
		case "resources/read":
			reads.add(req.Params.URI)
			text, _ := json.Marshal("contents of " + req.Params.URI + " RESOURCE-CANARY")
			result = `"result":{"contents":[{"uri":` + quoteJSON(req.Params.URI) + `,"text":` + string(text) + `}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,%s}`, req.ID, result)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func quoteJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

// resourceRig is mentionRig with a connected MCP server named docs whose
// resource tools are in the session.
func resourceRig(t *testing.T, reads *readLog) (*agent.Loop, *agent.MemStore) {
	t.Helper()
	loop, store, _ := mentionRig(t)
	g := mcp.NewGateway()
	t.Cleanup(g.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if errs := g.Connect(ctx, []mcp.ServerConfig{{Name: "docs", URL: resourceServer(t, reads).URL, Enabled: true}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	for _, tl := range g.Tools() {
		loop.Tools.Add(tl)
	}
	return loop, store
}

// @server:uri attaches the resource, read through the server's tool as the
// person: requested, approved and observed in the record, and recorded as
// an input.mention.
func TestResourceMentionAttachesThroughTheTool(t *testing.T) {
	var reads readLog
	loop, store := resourceRig(t, &reads)
	msg, atts, err := mentionExpander{}.Expand(context.Background(), loop, "summarise @docs:docs://guide please")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg.Text, "contents of docs://guide") || !strings.Contains(msg.Text, `<resource-`) || len(atts) != 1 || atts[0].Path != "mcp:docs:docs://guide" {
		t.Fatalf("text %q atts %+v", msg.Text, atts)
	}
	if got := reads.all(); len(got) != 1 || got[0] != "docs://guide" {
		t.Fatalf("reads %v", got)
	}
	reqs := storeEventsOf(t, store, agent.EvActionRequested)
	if len(reqs) != 1 || reqs[0].Actor != agent.ActorUser || !strings.Contains(string(reqs[0].Payload), "mcp__docs__read_resource") {
		t.Fatalf("not recorded as the person's read: %+v", reqs)
	}
	if len(storeEventsOf(t, store, agent.EvObservation)) != 1 {
		t.Fatal("the read's result is not in the record")
	}
	// A mention of a server with no such tool, or no URI scheme, stays as text.
	for _, typed := range []string{"ask @nobody:docs://x", "see @docs:guide"} {
		msg, atts, err := mentionExpander{}.Expand(context.Background(), loop, typed)
		if err != nil || len(atts) != 0 || msg.Text != typed {
			t.Fatalf("%s: %q %v %v", typed, msg.Text, atts, err)
		}
	}
	if got := reads.all(); len(got) != 1 {
		t.Fatalf("reads %v", got)
	}
}

// A deny rule on the server's tools, or on the URI, refuses the mention:
// nothing is read and the message is not sent.
func TestResourceMentionFollowsPolicy(t *testing.T) {
	for _, rule := range []string{"mcp__docs__*", "mcp__docs__read_resource(docs://private/*)"} {
		var reads readLog
		loop, store := resourceRig(t, &reads)
		if err := loop.Policy.AddDeny(rule); err != nil {
			t.Fatal(err)
		}
		msg, _, err := mentionExpander{}.Expand(context.Background(), loop, "look at @docs:docs://private/plan")
		if err == nil || strings.Contains(msg.Text, "CANARY") || len(reads.all()) != 0 {
			t.Fatalf("%s: a denied read happened: %q %v reads %v", rule, msg.Text, err, reads.all())
		}
		if len(storeEventsOf(t, store, agent.EvActionDenied)) != 1 {
			t.Fatalf("%s: the denial is not recorded", rule)
		}
	}
	// The URI rule leaves other resources readable.
	var reads readLog
	loop, _ := resourceRig(t, &reads)
	if err := loop.Policy.AddDeny("mcp__docs__read_resource(docs://private/*)"); err != nil {
		t.Fatal(err)
	}
	if _, atts, err := (mentionExpander{}).Expand(context.Background(), loop, "@docs:docs://public/a"); err != nil || len(atts) != 1 {
		t.Fatalf("a public resource: %v %v", atts, err)
	}
}

// In an editor's session an @server:uri in the prompt's text is read as the
// terminal reads it, reaches the model, and is recorded; a deny rule on the
// server's tools refuses the prompt with nothing read.
func TestStudioPromptReadsResourceMentions(t *testing.T) {
	for _, deny := range []bool{false, true} {
		var reads readLog
		srv := resourceServer(t, &reads)
		m := newScriptModel(t)
		perms := ""
		if deny {
			perms = `,"permissions":{"deny":["mcp__docs__*"]}`
		}
		acpModelsEnv(t, `{"model":{"default":"m","providers":{"m":`+stubProvider(m.URL, "m-1")+`}},`+
			`"mcp":{"servers":[{"name":"docs","url":"`+srv.URL+`","enabled":true}]}`+perms+`}`)
		ws := t.TempDir()
		r := &studioRig{t: t, ws: ws, model: m}
		r.cl = newStudioClient(t, ws, true)
		r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
		id := r.open()
		res := r.cl.call("session/prompt", map[string]any{"sessionId": id,
			"prompt": []any{map[string]any{"type": "text", "text": "summarise @docs:docs://guide"}}})
		if deny {
			if res.Error == nil || len(reads.all()) != 0 {
				t.Fatalf("a denied resource was read: %+v %v", res.Error, reads.all())
			}
			continue
		}
		if res.Error != nil {
			t.Fatalf("%+v", res.Error)
		}
		m.mu.Lock()
		body := strings.Join(m.bodies, "\n")
		m.mu.Unlock()
		if !strings.Contains(body, "contents of docs://guide") {
			t.Fatalf("the model was not given the resource: %.600s", body)
		}
		mentions, _ := r.recorded(id, agent.EvInputMention)
		if len(mentions) != 1 || mentions[0]["path"] != "mcp:docs:docs://guide" {
			t.Fatalf("mentions %v", mentions)
		}
	}
}
