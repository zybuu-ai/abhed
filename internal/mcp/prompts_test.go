package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// promptServer is an HTTP MCP server offering prompts when withPrompts is
// set; it records the methods it was asked.
func promptServer(t *testing.T, withPrompts bool) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		asked = append(asked, req.Method)
		mu.Unlock()
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := resultFor(req.Method)
		switch req.Method {
		case "initialize":
			if withPrompts {
				result = `"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{},"prompts":{}}}`
			}
		case "prompts/list":
			result = `"result":{"prompts":[{"name":"summarise","description":"Summarise a file","arguments":[{"name":"path","required":true}]},` +
				`{"name":"bad\u202ename"},{"name":"badarg","arguments":[{"name":"a b"}]}]}`
		case "prompts/get":
			var p struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			text, _ := json.Marshal("Summarise " + p.Arguments["path"] + " briefly.")
			result = `"result":{"messages":[{"role":"user","content":{"type":"text","text":` + string(text) + `}},` +
				`{"role":"user","content":{"type":"image","data":"xx"}}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,%s}`, req.ID, result)
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

// A server's prompts are listed with bad names left out and a warning, and
// prompts/get returns the text of its messages.
func TestPromptsListAndGet(t *testing.T) {
	srv, _ := promptServer(t, true)
	var warned bytes.Buffer
	old := WarnOut
	WarnOut = &warned
	t.Cleanup(func() { WarnOut = old })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := NewGateway()
	defer g.Close()
	if errs := g.Connect(ctx, []ServerConfig{{Name: "docs", URL: srv.URL, Enabled: true}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	ps := g.Prompts()
	if len(ps) != 1 || ps[0].Server != "docs" || ps[0].Prompt.Name != "summarise" || !ps[0].Prompt.Arguments[0].Required {
		t.Fatalf("prompts %+v", ps)
	}
	if !strings.Contains(warned.String(), `"bad\u202ename"`) || !strings.Contains(warned.String(), `"badarg"`) || strings.Contains(warned.String(), "\u202e") {
		t.Fatalf("warnings: %s", warned.String())
	}
	text, err := g.GetPrompt(ctx, "docs", "summarise", map[string]string{"path": "main.go"})
	if err != nil || text != "Summarise main.go briefly." {
		t.Fatalf("get: %q %v", text, err)
	}
	if _, err := g.GetPrompt(ctx, "nope", "summarise", nil); err == nil {
		t.Fatal("a prompt from an unknown server")
	}
}

// A server that does not declare prompts is never asked for them.
func TestPromptsOnlyWhenDeclared(t *testing.T) {
	srv, asked := promptServer(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := NewGateway()
	defer g.Close()
	if errs := g.Connect(ctx, []ServerConfig{{Name: "plain", URL: srv.URL, Enabled: true}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(g.Prompts()) != 0 || strings.Contains(strings.Join(*asked, ","), "prompts/list") {
		t.Fatalf("asked %v", *asked)
	}
}
