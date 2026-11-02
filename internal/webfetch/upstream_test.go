package webfetch

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
	"github.com/zybuu-ai/abhed/internal/managed"
)

// Outside the allowlist web_fetch goes out through the upstream proxy, its
// own dialer reaching the proxy as the operator named it. A name that does
// not resolve here is refused by default; with resolve_via_proxy it is sent
// only when the managed allowed_hosts names it exactly. A name that resolves here to an
// internal address is refused before the proxy.
func TestWebFetchThroughTheUpstreamProxy(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "via the corporate proxy")
	}))
	corp := upstreamtest.Start(t)
	up := corp.Use(t, "svc", "pw")
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	page := "http://public.example.test:" + port + "/p"
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("an unresolved name with the switch off: %s", res.Content)
	}
	up.ResolveViaProxy = true
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("an unresolved name with no allowed_hosts: %s", res.Content)
	}
	tool.AllowedHosts = []string{"*.example.test"}
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("an unresolved name allowed only by a wildcard: %s", res.Content)
	}
	// Named exactly in the effective list but not by the managed file: a
	// lower layer's narrowing of a managed wildcard anchors nothing.
	tool.AllowedHosts = []string{"public.example.test", "internal.test"}
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("an unresolved name the managed list does not name exactly: %s", res.Content)
	}
	tool.ExactHosts = []string{"public.example.test", "internal.test"}
	res := run(t, tool, page)
	if res.IsError || !strings.Contains(res.Content, "via the corporate proxy") || !corp.Saw("public.example.test") {
		t.Fatalf("through the proxy: %s", res.Content)
	}
	res = run(t, tool, "http://internal.test:"+port+"/")
	if !res.IsError || corp.Saw("internal.test") {
		t.Fatalf("an internal name through the proxy: %s", res.Content)
	}
}

// The reviewer's probe: a managed wildcard that a trusted lower layer
// narrows to one exact host anchors nothing, so with resolve_via_proxy on
// the unresolved name is refused and the proxy is never asked. A host the
// managed file names exactly does anchor.
func TestNarrowedManagedWildcardAnchorsNothing(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "via the corporate proxy")
	}))
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	load := func(managedBody string) config.Config {
		t.Helper()
		home, ws := t.TempDir(), t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		mf := filepath.Join(t.TempDir(), "managed.json")
		if err := os.WriteFile(mf, []byte(managedBody), 0o644); err != nil {
			t.Fatal(err)
		}
		old := managed.ConfigFile
		managed.ConfigFile = mf
		t.Cleanup(func() { managed.ConfigFile = old })
		if err := os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"),
			[]byte(`{"web_fetch":{"allowed_hosts":["public.example.test"]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadWith(ws, config.LoadOptions{Quiet: true, Trust: config.TrustGranted})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	corp := upstreamtest.Start(t)
	up := corp.Use(t, "svc", "pw")
	up.ResolveViaProxy = true
	page := "http://public.example.test:" + port + "/p"

	cfg := load(`{"web_fetch":{"enabled":true,"allowed_hosts":["*.example.test"]}}`)
	if !slices.Equal(cfg.WebFetch.AllowedHosts, []string{"public.example.test"}) || len(cfg.WebFetchAnchors()) != 0 {
		t.Fatalf("hosts %v, anchors %v", cfg.WebFetch.AllowedHosts, cfg.WebFetchAnchors())
	}
	tool.AllowedHosts, tool.ExactHosts = cfg.WebFetch.AllowedHosts, cfg.WebFetchAnchors()
	if res := run(t, tool, page); !res.IsError || corp.Saw("public.example.test") {
		t.Fatalf("a narrowed managed wildcard anchored the name: %s", res.Content)
	}

	cfg = load(`{"web_fetch":{"enabled":true,"allowed_hosts":["public.example.test","*.other.test"]}}`)
	if !slices.Equal(cfg.WebFetchAnchors(), []string{"public.example.test"}) {
		t.Fatalf("anchors %v", cfg.WebFetchAnchors())
	}
	tool.AllowedHosts, tool.ExactHosts = cfg.WebFetch.AllowedHosts, cfg.WebFetchAnchors()
	if res := run(t, tool, page); res.IsError || !corp.Saw("public.example.test") {
		t.Fatalf("a host the managed file names exactly: %s", res.Content)
	}
}
