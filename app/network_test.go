package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/profiles"
)

const testProxy = "proxy.corp.test:3128"

// networkHome is a home whose own configuration names network, with no
// managed file and no proxy in force; it returns a workspace.
func networkHome(t *testing.T, network string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv(config.TrustEnv, "")
	old := managed.ConfigFile
	managed.ConfigFile = filepath.Join(t.TempDir(), "absent.json")
	t.Cleanup(func() { managed.ConfigFile = old })
	egress.ResetUpstreamForTest()
	t.Cleanup(egress.ResetUpstreamForTest)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"network":`+network+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return t.TempDir()
}

func proxyInForce() string {
	if up := egress.CurrentUpstream(); up != nil {
		return up.Authority()
	}
	return ""
}

// Every command that makes a connection of Abhed's own puts the configured
// upstream proxy in force before it connects.
func TestNetworkedCommandsPutTheProxyInForce(t *testing.T) {
	for name, run := range map[string]func(ws string) error{
		"record list": func(ws string) error {
			recordCmd(ws, []string{"list"}, config.TrustRefused, strings.NewReader(""), io.Discard, io.Discard)
			return nil
		},
		"record ship status": func(ws string) error {
			recordCmd(ws, []string{"ship", "status"}, config.TrustRefused, strings.NewReader(""), io.Discard, io.Discard)
			return nil
		},
		"hawkeye store": func(ws string) error {
			_, done, err := hawkeyeStore(ws, config.TrustRefused)
			if err == nil {
				done()
			}
			return err
		},
		"policy refresh": func(string) error {
			policyCmd([]string{"refresh"}, io.Discard, io.Discard)
			return nil
		},
		"mcp list": func(ws string) error { return mcpList(ws, config.TrustRefused, io.Discard) },
		"sandbox list": func(ws string) error {
			sandboxCmd(ws, []string{"list"}, strings.NewReader(""), io.Discard, io.Discard)
			return nil
		},
		"profile list": func(ws string) error {
			profileCmd(ws, []string{"list"}, io.Discard, io.Discard)
			return nil
		},
		"session start": func(ws string) error {
			return orgPolicyAfterNetwork(context.Background(), ws, config.LoadOptions{Quiet: true}, io.Discard)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws := networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`"}}`)
			fetchOrgAtStart = func(context.Context, io.Writer) {}
			t.Cleanup(func() { fetchOrgAtStart = orgPolicyAtStart })
			if err := run(ws); err != nil {
				t.Fatal(err)
			}
			if got := proxyInForce(); got != testProxy {
				t.Fatalf("the proxy in force is %q", got)
			}
		})
	}
}

// At a session's start the proxy goes in force before the org policy is
// fetched; a network configuration that cannot go in force fetches nothing.
func TestOrgPolicyFetchFollowsTheNetwork(t *testing.T) {
	ws := networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`"}}`)
	var seen []string
	fetchOrgAtStart = func(context.Context, io.Writer) { seen = append(seen, proxyInForce()) }
	t.Cleanup(func() { fetchOrgAtStart = orgPolicyAtStart })
	if err := orgPolicyAfterNetwork(context.Background(), ws, config.LoadOptions{Quiet: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != testProxy {
		t.Fatalf("the fetch ran with %v in force", seen)
	}

	seen = nil
	ws = networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`","ca_file":"/nonexistent/corp.pem"}}`)
	if err := orgPolicyAfterNetwork(context.Background(), ws, config.LoadOptions{Quiet: true}, io.Discard); err == nil || len(seen) != 0 {
		t.Fatalf("a broken network configuration: %v, fetched %v", err, seen)
	}
}

// An editor connection whose network configuration cannot go in force
// starts neither the org policy fetch nor its periodic refresh, and its
// sessions fetch nothing either; one that can starts both.
func TestACPConnectOrgPolicyFollowsTheNetwork(t *testing.T) {
	var fetched, refreshing int
	fetchOrgAtStart = func(context.Context, io.Writer) { fetched++ }
	startOrgRefresh = func(context.Context, time.Duration, io.Writer) { refreshing++ }
	t.Cleanup(func() { fetchOrgAtStart, startOrgRefresh = orgPolicyAtStart, keepOrgPolicyFresh })

	networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`","ca_file":"/nonexistent/corp.pem"}}`)
	c := &acpConn{ctx: context.Background(), out: io.Discard, sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	c.orgPolicyAtConnect(io.Discard)
	if !c.noNetwork || fetched != 0 || refreshing != 0 || proxyInForce() != "" {
		t.Fatalf("noNetwork %v, fetched %d, refreshing %d, proxy %q", c.noNetwork, fetched, refreshing, proxyInForce())
	}
	if _, e := c.openSession(openOptions{cwd: t.TempDir()}); e == nil || fetched != 0 {
		t.Fatalf("a session opened (%v) or fetched (%d) around the proxy", e, fetched)
	}

	networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`"}}`)
	c = &acpConn{ctx: context.Background(), out: io.Discard, sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	c.orgPolicyAtConnect(io.Discard)
	if c.noNetwork || fetched != 1 || refreshing != 1 || proxyInForce() != testProxy {
		t.Fatalf("noNetwork %v, fetched %d, refreshing %d, proxy %q", c.noNetwork, fetched, refreshing, proxyInForce())
	}
}

// Through an upstream proxy the CLI's endpoint probe never resolves or dials
// the model host itself: the gated adapter's call goes to the proxy, which
// reaches a model this machine cannot resolve. A no_proxy host is probed.
func TestEndpointProbeLeavesTheModelToTheProxy(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"through the proxy\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(llm.Close)
	port := llm.URL[strings.LastIndex(llm.URL, ":")+1:]
	var dialled []string
	old := probeDial
	probeDial = func(_ context.Context, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
	}
	t.Cleanup(func() { probeDial = old })
	egress.ResetUpstreamForTest()
	t.Cleanup(egress.ResetUpstreamForTest)
	corp := upstreamtest.Start(t)
	up := corp.Use(t, "svc", "pw")
	up.ResolveViaProxy = true

	p := config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://llm.corp.test:" + port + "/v1", Model: "m", ContextWindow: 8192}
	probe := newEndpointProbe(p)
	probe.start(context.Background())
	if err := probe.firstResult(2 * time.Second); err != nil {
		t.Fatalf("the probe: %v", err)
	}
	g := gatedAdapter{Adapter: buildAdapter(p), probe: probe}
	ch, err := g.Complete(context.Background(), model.Request{Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	for c := range ch {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		text += c.Text
	}
	if text != "through the proxy" || !corp.Saw("llm.corp.test") || len(dialled) != 0 {
		t.Fatalf("answer %q, proxy saw it %v, dialled directly %v", text, corp.Saw("llm.corp.test"), dialled)
	}

	up.NoProxy = []string{"llm.corp.test"}
	if err := newEndpointProbe(p).run(context.Background()); err == nil || len(dialled) != 1 {
		t.Fatalf("a no_proxy model host was not probed: %v %v", err, dialled)
	}
}

// The record's witness, opened as abhed record opens it, posts each head
// through the configured upstream proxy, and a second stand-in named by
// HTTP_PROXY sees nothing.
func TestWitnessPostsThroughTheUpstreamProxy(t *testing.T) {
	var got atomic.Int32
	wsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got.Add(1)
		}
	}))
	t.Cleanup(wsrv.Close)
	port := wsrv.URL[strings.LastIndex(wsrv.URL, ":")+1:]
	corp, other := upstreamtest.Start(t), upstreamtest.Start(t)
	ws := networkHome(t, `{}`)
	t.Setenv("HTTP_PROXY", "http://"+other.Addr())
	mf := filepath.Join(t.TempDir(), "managed.json")
	body := `{"network":{"upstream_proxy":{"url":"http://` + corp.Addr() + `"}},` +
		`"record":{"witness":{"url":"http://witness.corp.test:` + port + `/heads"}}}`
	if err := os.WriteFile(mf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = mf
	cfg, err := loadNetworked(ws, config.LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	w, err := openWitness(cfg)
	if err != nil || w == nil {
		t.Fatalf("witness %v: %v", w, err)
	}
	w.Local("t", "s-1", 3, 3, strings.Repeat("ab", 32))
	w.Close(5 * time.Second)
	if got.Load() != 1 || !corp.Saw("witness.corp.test") || other.Saw("witness.corp.test") {
		t.Fatalf("posts %d, configured proxy %v, environment's %v", got.Load(), corp.Saw("witness.corp.test"), other.Saw("witness.corp.test"))
	}
}

// A model call that fails at the upstream proxy names the proxy and the
// host, for the proxy's refusal of a tunnel and for a 502 through it.
func TestModelFailureThroughTheProxyNamesIt(t *testing.T) {
	egress.ResetUpstreamForTest()
	t.Cleanup(egress.ResetUpstreamForTest)
	corp := upstreamtest.Start(t)
	corp.Use(t, "", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	p := config.ProviderConfig{Type: "openai-compatible", BaseURL: fmt.Sprintf("https://llm.corp.test:%d/v1", closed), Model: "m"}
	// The error Go's own CONNECT gives for the proxy's 502.
	resp, cerr := (&http.Client{Transport: egress.UpstreamRoundTripper{}, Timeout: 5 * time.Second}).Get(p.BaseURL + "/models")
	if cerr == nil {
		_ = resp.Body.Close()
		t.Fatal("a closed port answered")
	}
	for name, e := range map[string]error{
		"tunnel":   cerr,
		"refused":  &egress.UpstreamRefused{Proxy: corp.Addr(), Target: "llm.corp.test:443", Status: http.StatusBadGateway},
		"http 502": &model.StatusError{Status: http.StatusBadGateway, Attempts: 4},
		"http 504": &model.StatusError{Status: http.StatusGatewayTimeout, Attempts: 4},
	} {
		got := friendlyModelError(e, "m", p)
		if !strings.Contains(got, "upstream proxy "+corp.Addr()) || !strings.Contains(got, "llm.corp.test") {
			t.Errorf("%s: %s", name, got)
		}
	}
	// The model server's own answers, and other errors, are never put on the proxy.
	for name, e := range map[string]error{
		"server 403 Forbidden":       fmt.Errorf("model: %w", &model.StatusError{Status: http.StatusForbidden, Body: "Forbidden", Attempts: 1}),
		"server 400 ... Bad Gateway": &model.StatusError{Status: http.StatusBadRequest, Body: "upstream said Bad Gateway", Attempts: 1},
		"other : Forbidden":          errors.New("open /x: Forbidden"),
		"url error : Forbidden":      &url.Error{Op: "Post", URL: p.BaseURL, Err: fmt.Errorf("read: %w", errors.New("Forbidden"))},
	} {
		if got := friendlyModelError(e, "m", p); strings.Contains(got, "upstream proxy") {
			t.Errorf("%s was put on the proxy: %s", name, got)
		}
	}
	if got := friendlyModelError(&model.StatusError{Status: http.StatusForbidden, Body: "Forbidden", Attempts: 1}, "m", p); !strings.Contains(got, "check the key") {
		t.Errorf("a server 403 lost the key advice: %s", got)
	}
	// The proxy down, and asking for credentials.
	dl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := dl.Addr().String()
	_ = dl.Close()
	egress.SetUpstream(&egress.Upstream{URL: &url.URL{Scheme: "http", Host: addr}})
	_, derr := (&http.Client{Transport: egress.UpstreamRoundTripper{}, Timeout: 5 * time.Second}).Get(p.BaseURL + "/models") //nolint:bodyclose // fails
	for name, e := range map[string]error{
		"go proxyconnect": derr,
		"own dial":        &egress.UpstreamUnreachable{Proxy: addr, Err: errors.New("connect: connection refused")},
	} {
		if got := friendlyModelError(e, "m", p); !strings.Contains(got, "upstream proxy "+addr+" is not answering") || strings.Contains(got, "Start the model server") {
			t.Errorf("%s: %s", name, got)
		}
	}
	for name, e := range map[string]error{
		"refused 407": &egress.UpstreamRefused{Proxy: addr, Target: "llm.corp.test:443", Status: http.StatusProxyAuthRequired},
		"go 407":      &url.Error{Op: "Post", URL: p.BaseURL, Err: errors.New("Proxy Authentication Required")},
	} {
		if got := friendlyModelError(e, "m", p); !strings.Contains(got, "credentials_secret") {
			t.Errorf("%s: %s", name, got)
		}
	}
	// A provider's default endpoint goes through the proxy too, and is named so.
	if got := friendlyModelError(&model.StatusError{Status: http.StatusBadGateway, Attempts: 4}, "m",
		config.ProviderConfig{Type: "anthropic", Model: "m"}); !strings.Contains(got, "upstream proxy "+addr) {
		t.Errorf("default endpoint: %s", got)
	}
	// Directly, nothing about a proxy is said.
	egress.SetUpstream(nil)
	if got := friendlyModelError(&model.StatusError{Status: http.StatusBadGateway, Attempts: 4}, "m", p); strings.Contains(got, "proxy") {
		t.Errorf("direct: %s", got)
	}
}

// Record shipping and the org policy fetch reach their server through the
// configured upstream proxy, and a proxy named by HTTP_PROXY sees nothing.
func TestShippingAndOrgFetchGoThroughTheUpstreamProxy(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	server := "http://org.corp.test:" + port
	corp, other := upstreamtest.Start(t), upstreamtest.Start(t)
	networkHome(t, `{"upstream_proxy":{"url":"http://`+corp.Addr()+`"}}`)
	t.Setenv("HTTP_PROXY", "http://"+other.Addr())
	if !profileNetwork(io.Discard) {
		t.Fatal("the network configuration did not go in force")
	}
	pf := filepath.Join(t.TempDir(), "profiles.json")
	p := &profiles.Profile{URL: server, Token: "tok", TokenType: "Bearer"}
	if err := profiles.Save(pf, &profiles.File{Version: 1, Current: "corp", Profiles: map[string]*profiles.Profile{"corp": p}}); err != nil {
		t.Fatal(err)
	}
	ship := shipTarget{url: server, profile: "corp", profilesFile: pf}.doer()
	if _, _, err := ship(context.Background(), http.MethodPost, "/v1/records", "application/json", []byte("{}")); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if !corp.Saw(server+"/v1/records") || other.Saw("org.corp.test") {
		t.Fatalf("shipping: configured proxy %v, environment's %v", corp.Saw(server+"/v1/records"), other.Saw("org.corp.test"))
	}
	if _, err := orgFetch(context.Background(), pf, "corp", p); !errors.Is(err, errNoOrgPolicy) {
		t.Fatalf("org fetch: %v", err)
	}
	if !corp.Saw(server+orgPolicyPath) || other.Saw("org.corp.test") || hits.Load() != 2 {
		t.Fatalf("org fetch: configured proxy %v, environment's %v, server %d", corp.Saw(server+orgPolicyPath), other.Saw("org.corp.test"), hits.Load())
	}
}

// /doctor through a proxy says the endpoint answers only after a call came
// back through it, and names the proxy; a failed call shows the error alone.
func TestDoctorEndpointRowThroughTheProxy(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(llm.Close)
	port := llm.URL[strings.LastIndex(llm.URL, ":")+1:]
	egress.ResetUpstreamForTest()
	t.Cleanup(egress.ResetUpstreamForTest)
	corp := upstreamtest.Start(t)
	corp.Use(t, "", "").ResolveViaProxy = true
	p := config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://llm.corp.test:" + port + "/v1", Model: "m", ContextWindow: 8192}
	if got := endpointRow(context.Background(), p, "m"); got != "answers through the upstream proxy "+corp.Addr()+", but the model did not call the tool it was given" {
		t.Fatalf("an answer through the proxy: %q", got)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	p.BaseURL = fmt.Sprintf("https://llm.corp.test:%d/v1", closed)
	got := endpointRow(context.Background(), p, "m")
	if strings.Contains(got, "answers") || !strings.Contains(got, "upstream proxy "+corp.Addr()) {
		t.Fatalf("a failed call through the proxy: %q", got)
	}
}
