package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// A remote tool whose name holds anything but letters, digits, _ . and -, or
// is longer than 64, is not registered, and a warning names it escaped.
func TestRemoteToolNamesAreValidated(t *testing.T) {
	long := strings.Repeat("x", 65)
	list, _ := json.Marshal(map[string]any{"tools": []map[string]any{
		{"name": "search"}, {"name": "look\u202eup"}, {"name": "a b"}, {"name": long},
		{"name": "ok.name-1_" + strings.Repeat("y", 54)}, {"name": ""},
	}})
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
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := resultFor(req.Method)
		if req.Method == "tools/list" {
			result = `"result":` + string(list)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,%s}`, req.ID, result)
	}))
	defer srv.Close()
	var warned bytes.Buffer
	old := WarnOut
	WarnOut = &warned
	t.Cleanup(func() { WarnOut = old })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g := NewGateway()
	defer g.Close()
	if errs := g.Connect(ctx, []ServerConfig{{Name: "hid", URL: srv.URL, Enabled: true}}); len(errs) > 0 {
		t.Fatal(errs)
	}
	var names []string
	for _, tool := range g.Tools() {
		names = append(names, tool.Name())
	}
	want := []string{"mcp__hid__search", "mcp__hid__ok.name-1_" + strings.Repeat("y", 54)}
	if !slices.Equal(names, want) {
		t.Fatalf("registered %q, want %q", names, want)
	}
	out := warned.String()
	for _, name := range []string{`"look\u202eup"`, `"a b"`, `"` + long + `"`, `""`} {
		if !strings.Contains(out, `mcp server "hid" offers a tool named `+name) {
			t.Errorf("no warning names %s:\n%s", name, out)
		}
	}
	if strings.Contains(out, "\u202e") {
		t.Errorf("the warning carries the raw RLO:\n%s", out)
	}
}
