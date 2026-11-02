package sandboxcli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/egress/upstreamtest"
	"github.com/zybuu-ai/abhed/profiles"
)

// With an upstream proxy in force, a REST call goes to the server through it
// and a stream tunnels through it with CONNECT; a proxy named by HTTP_PROXY
// sees neither, and the server's name is never dialled directly.
func TestCallsAndStreamsGoThroughTheUpstreamProxy(t *testing.T) {
	r := newRig(t)
	sb := r.ready(t)
	port := r.url[strings.LastIndex(r.url, ":")+1:]
	f, err := profiles.Load(r.profiles)
	if err != nil {
		t.Fatal(err)
	}
	server := "http://abhed.corp.test:" + port
	for _, p := range f.Profiles {
		p.URL = server
	}
	if err := profiles.Save(r.profiles, f); err != nil {
		t.Fatal(err)
	}
	other := upstreamtest.Start(t)
	t.Setenv("HTTP_PROXY", "http://"+other.Addr())
	corp := upstreamtest.Start(t)
	corp.Use(t, "svc", "pw")

	if res := r.must(t, "list"); !strings.Contains(res.out, sb.ID) {
		t.Fatalf("list: %+v", res)
	}
	if !corp.Saw(server + "/") {
		t.Fatal("the REST call did not go through the upstream proxy")
	}
	// -i: stdin over the WebSocket.
	if res := r.runWith(t, runOpts{in: strings.NewReader("x")}, "exec", "-i", sb.ID, "--", "cat"); res.code != 0 {
		t.Fatalf("exec -i: %+v", res)
	}
	if !corp.SawExactly("abhed.corp.test:" + port) {
		t.Fatal("the stream did not tunnel through the upstream proxy")
	}
	if other.Saw("abhed.corp.test") {
		t.Fatal("the environment's proxy was used")
	}
}

// Behind a proxy that inspects TLS, the server's certificate is signed by
// the organisation's CA, which network.upstream_proxy.ca_file names: the
// REST calls and the streams both trust it, and a stream without it fails.
func TestStreamsTrustTheUpstreamBundle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such stream", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	cert := srv.Certificate() // stands in for the re-signing CA
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	pf := filepath.Join(t.TempDir(), "profiles.json")
	if err := profiles.Save(pf, &profiles.File{Version: 1, Current: "corp", Profiles: map[string]*profiles.Profile{
		"corp": {URL: "https://example.com:" + port, Token: aliceToken, TokenType: "Bearer"}}}); err != nil {
		t.Fatal(err)
	}
	corp := upstreamtest.Start(t)
	up := corp.Use(t, "", "")
	stream := func() error {
		t.Helper()
		cl, err := openClient(pf, "", "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = cl.dial(context.Background(), "/v1/stream", "abhed.test")
		return err
	}
	if err := stream(); err == nil || !strings.Contains(err.Error(), "x509") {
		t.Fatalf("without the bundle the stream verified: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	up.Roots, up.Bundle = pool, []*x509.Certificate{cert}
	cl, err := openClient(pf, "", "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cl.hc.Get("https://example.com:" + port + "/v1/sandboxes")
	if err != nil {
		t.Fatalf("REST with the bundle: %v", err)
	}
	_ = resp.Body.Close()
	if err := stream(); err == nil || strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "could not reach") {
		t.Fatalf("the stream with the bundle did not reach the server's answer: %v", err)
	}
	if !corp.SawExactly("example.com:" + port) {
		t.Fatal("the stream did not tunnel through the proxy")
	}
	// A server the proxy bypasses is reached as before, without the bundle.
	up.NoProxy = []string{"example.com"}
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	if got := egress.UpstreamTLS("example.com", base); got != base {
		t.Fatal("a no_proxy server was given the bundle")
	}
}
