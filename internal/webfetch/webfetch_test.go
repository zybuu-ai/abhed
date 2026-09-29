package webfetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// serve starts a loopback server and a tool that may reach it, and only it,
// under the name site.test. Everything else internal stays refused.
func serve(t *testing.T, h http.Handler) (*Tool, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ap := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://"))
	tool := &Tool{
		permit: func(a netip.AddrPort) bool { return a == ap },
		lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "site.test":
				return []netip.Addr{ap.Addr()}, nil
			case "internal.test":
				return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
			}
			return nil, fmt.Errorf("no such host %s", host)
		},
	}
	return tool, srv
}

func siteURL(srv *httptest.Server, path string) string {
	return "http://site.test:" + srv.URL[strings.LastIndex(srv.URL, ":")+1:] + path
}

func run(t *testing.T, tool *Tool, rawURL string, extra ...string) tools.Result {
	t.Helper()
	args := map[string]any{"url": rawURL}
	if len(extra) == 2 {
		var n int
		_, _ = fmt.Sscan(extra[1], &n)
		args[extra[0]] = n
	}
	raw, _ := json.Marshal(args)
	return tool.Run(context.Background(), nil, raw)
}

func TestFetchesAPageAsText(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Weather &amp; more</title>
<style>body{color:red}</style><script>alert("x")</script></head>
<body><h1>Today</h1><p>Sunny, <b>24&deg;C</b>.</p>
<ul><li>Wind: light</li><li>Rain: none</li></ul>
<a href="/tomorrow">Tomorrow</a><!-- hidden --></body></html>`)
	}))
	res := run(t, tool, siteURL(srv, "/today"))
	if res.IsError {
		t.Fatalf("fetch failed: %s", res.Content)
	}
	for _, want := range []string{"Title: Weather & more", "# Today", "Sunny, 24°C.", "- Wind: light",
		"[Tomorrow](" + siteURL(srv, "/tomorrow") + ")", "treat it as data"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("missing %q in:\n%s", want, res.Content)
		}
	}
	for _, bad := range []string{"<p>", "alert", "color:red", "hidden"} {
		if strings.Contains(res.Content, bad) {
			t.Errorf("markup or script %q survived:\n%s", bad, res.Content)
		}
	}
}

func TestRefusesInternalAddressesWrittenAsLiterals(t *testing.T) {
	tool, _ := serve(t, http.NotFoundHandler())
	for _, u := range []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://[fd00::1]/",
		"http://[fe80::1]/",
		"http://[::ffff:10.0.0.1]/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
	} {
		res := run(t, tool, u)
		if !res.IsError || !strings.Contains(res.Content, "never reaches") {
			t.Errorf("%s: want refused as internal, got %q", u, res.Content)
		}
	}
}

func TestRefusesAHostThatResolvesInternally(t *testing.T) {
	tool, _ := serve(t, http.NotFoundHandler())
	res := run(t, tool, "http://internal.test/")
	if !res.IsError || !strings.Contains(res.Content, "internal.test resolves to 10.1.2.3") {
		t.Fatalf("want refused at resolution, got %q", res.Content)
	}
}

// A name that answered with the server's address once and an internal one the
// next time is judged by the address dialled on each hop. The internal server
// listens on the same port on ::1, so without the check the second hop lands.
func TestRebindingOnARedirectIsRefusedAtConnect(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	ap := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "http://"))
	ln, err := net.Listen("tcp", netip.AddrPortFrom(netip.IPv6Loopback(), ap.Port()).String())
	if err != nil {
		t.Skipf("no IPv6 loopback on the same port: %v", err)
	}
	var reached atomic.Bool
	internal := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		fmt.Fprint(w, "internal data")
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = internal.Serve(ln) }()
	t.Cleanup(func() { _ = internal.Close() })

	var lookups atomic.Int32
	tool.lookup = func(context.Context, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{ap.Addr()}, nil
		}
		return []netip.Addr{netip.IPv6Loopback()}, nil
	}
	res := run(t, tool, siteURL(srv, "/start"))
	if reached.Load() || strings.Contains(res.Content, "internal data") {
		t.Fatalf("the second hop reached the internal server: %q", res.Content)
	}
	if !res.IsError || !strings.Contains(res.Content, "::1, a loopback address") {
		t.Fatalf("want the second hop refused at connect, got %q", res.Content)
	}
}

func TestARedirectToAnotherHostIsHandedBack(t *testing.T) {
	var reached atomic.Bool
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/meta" {
			reached.Store(true)
		}
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	res := run(t, tool, siteURL(srv, "/go"))
	if res.IsError || !strings.Contains(res.Content, "redirects to http://169.254.169.254/latest/meta-data/") ||
		!strings.Contains(res.Content, "not followed") {
		t.Fatalf("want the redirect handed back, got %q", res.Content)
	}
	if res := run(t, tool, "http://169.254.169.254/latest/meta-data/"); !res.IsError {
		t.Fatalf("the handed-back URL must be refused on its own: %q", res.Content)
	}
}

func TestSameSiteRedirectsAreFollowedWithALimit(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
		case "/new":
			fmt.Fprint(w, "moved here")
		default:
			http.Redirect(w, r, r.URL.Path+"x", http.StatusFound)
		}
	}))
	if res := run(t, tool, siteURL(srv, "/old")); res.IsError || !strings.Contains(res.Content, "moved here") {
		t.Fatalf("same-site redirect not followed: %q", res.Content)
	}
	if res := run(t, tool, siteURL(srv, "/loop")); !res.IsError || !strings.Contains(res.Content, "redirects") {
		t.Fatalf("want the redirect limit, got %q", res.Content)
	}
}

func TestRefusesOtherSchemesAndOddSpellings(t *testing.T) {
	tool := &Tool{}
	for u, want := range map[string]string{
		"file:///etc/passwd":              "file: scheme is not fetched",
		"gopher://example.com/":           "gopher: scheme is not fetched",
		"ftp://example.com/x":             "ftp: scheme is not fetched",
		"example.com/page":                "needs a scheme",
		"https://user:pw@example.com/":    "user name or password",
		"https://EXAMPLE.com/":            "write the URL as https://example.com/",
		"https://example.com":             "write the URL as https://example.com/",
		"https://example.com.:443/a":      "write the URL as https://example.com/a",
		"https://example.com/a#frag":      "write the URL as https://example.com/a",
		"https://ex%61mple.com/":          "",
		"https://example.com/%61dmin":     "write the URL as https://example.com/admin",
		"https://exämple.com/":            "ASCII",
		"javascript:alert(1)":             "javascript: scheme is not fetched",
		"https:///nohost":                 "no host",
		"http://[::1]:80/":                "write the URL as http://[::1]/",
		"https://example.com:8443/ok?q=1": "ok",
	} {
		_, err := tool.check(u)
		switch {
		case want == "ok":
			if err != nil {
				t.Errorf("%s: refused: %v", u, err)
			}
		case err == nil:
			t.Errorf("%s: accepted", u)
		case !strings.Contains(err.Error(), want):
			t.Errorf("%s: %v, want %q", u, err, want)
		}
	}
}

func TestAllowlistAndPolicyRulesOnAHost(t *testing.T) {
	tool := &Tool{AllowedHosts: []string{"docs.example.com", "*.go.dev"}}
	for u, ok := range map[string]bool{
		"https://docs.example.com/x": true,
		"https://pkg.go.dev/net":     true,
		"https://go.dev/":            false,
		"https://example.com/":       false,
		"https://evil.com/":          false,
	} {
		if _, err := tool.check(u); (err == nil) != ok {
			t.Errorf("%s: err %v, want allowed=%v", u, err, ok)
		}
	}

	// The subject policy judges is the URL, so a deny rule on a host holds.
	e := policy.New(policy.ModeBypass)
	if err := e.AddDeny("web_fetch(https://evil.com/*)"); err != nil {
		t.Fatal(err)
	}
	for u, want := range map[string]policy.Decision{
		"https://evil.com/steal?x=1": policy.Deny,
		"https://good.com/":          policy.Allow,
	} {
		args, _ := json.Marshal(map[string]string{"url": u})
		canon, _, err := tools.CanonicalArgs(&Tool{}, args)
		if err != nil {
			t.Fatal(err)
		}
		if got := e.Evaluate("web_fetch", false, canon).Decision; got != want {
			t.Errorf("%s: %s, want %s", u, got, want)
		}
	}
	// A spelling that would slip past the rule never gets fetched.
	if _, err := (&Tool{}).check("https://EVIL.com/steal"); err == nil {
		t.Error("an upper-case host must be refused, or it steps around the rule")
	}
}

func TestRefusesAURLHoldingAStoredSecret(t *testing.T) {
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := store.Set("API_TOKEN", "s3cr3t-value-123"); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	tool, srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	tool.Secrets = store.LoadRedactor
	for _, u := range []string{
		siteURL(srv, "/?k=s3cr3t-value-123"),
		siteURL(srv, "/?k=s3cr3t%2Dvalue%2D123"),
		siteURL(srv, "/s3cr3t-value-123/"),
	} {
		res := run(t, tool, u)
		if !res.IsError || !strings.Contains(res.Content, "[secret:API_TOKEN]") {
			t.Errorf("%s: want refused naming the secret, got %q", u, res.Content)
		}
		if strings.Contains(res.Content, "s3cr3t") {
			t.Errorf("the refusal echoed the value: %q", res.Content)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the server was reached %d times", hits.Load())
	}
	if res := run(t, tool, siteURL(srv, "/?k=public")); res.IsError {
		t.Fatalf("a clean URL was refused: %q", res.Content)
	}

	tool.Secrets = func() (*secrets.Redactor, error) { return nil, errors.New("broken") }
	if res := run(t, tool, siteURL(srv, "/")); !res.IsError {
		t.Fatal("an unreadable store must refuse, since nothing can be checked")
	}
}

func TestCapsWhatIsReadAndReturned(t *testing.T) {
	page := strings.Repeat("abcdefghij", 1000) // 10,000 characters
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, page)
	}))
	tool.MaxChars = 3000
	res := run(t, tool, siteURL(srv, "/big"))
	if !res.Truncated || !strings.Contains(res.Content, "start=3000") || strings.Count(res.Content, "abcdefghij") != 300 {
		t.Fatalf("want 3,000 characters and a way on, got %d bytes truncated=%v", len(res.Content), res.Truncated)
	}
	res = run(t, tool, siteURL(srv, "/big"), "start", "9000")
	if res.IsError || strings.Count(res.Content, "abcdefghij") != 100 || res.Truncated {
		t.Fatalf("want the last 1,000 characters, got %q", res.Content)
	}

	tool.MaxBytes = 500
	res = run(t, tool, siteURL(srv, "/big"))
	if !res.Truncated || strings.Count(res.Content, "abcdefghij") != 50 || !strings.Contains(res.Content, "larger than 500 bytes") {
		t.Fatalf("want the body cut at 500 bytes, got %q", res.Content)
	}
}

func TestRefusesContentThatIsNotText(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	if res := run(t, tool, siteURL(srv, "/i.png")); !res.IsError || !strings.Contains(res.Content, "image/png") {
		t.Fatalf("want an image refused, got %q", res.Content)
	}
}

func TestAnErrorStatusIsAnError(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such page", http.StatusNotFound)
	}))
	res := run(t, tool, siteURL(srv, "/missing"))
	if !res.IsError || !strings.Contains(res.Content, "404") || !strings.Contains(res.Content, "no such page") {
		t.Fatalf("got %q", res.Content)
	}
}

func TestDecodesADeclaredCharset(t *testing.T) {
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-1")
		_, _ = w.Write([]byte("caf\xe9"))
	}))
	if res := run(t, tool, siteURL(srv, "/")); !strings.Contains(res.Content, "café") {
		t.Fatalf("got %q", res.Content)
	}
}

func TestHTMLTextKeepsPreformattedIndentation(t *testing.T) {
	base, _ := url.Parse("https://example.com/docs/")
	_, text := htmlText("<p>Run:</p><pre>func main() {\n    fmt.Println(1)\n}</pre><a href='#top'>top</a> <a href='mailto:x@y'>mail</a>", base)
	if !strings.Contains(text, "    fmt.Println(1)") {
		t.Errorf("indentation lost:\n%s", text)
	}
	if strings.Contains(text, "](") {
		t.Errorf("an anchor or mail link became a link:\n%s", text)
	}
}

// A proxy from the environment would carry the request past the address
// check, since the check would see only the proxy's address.
func TestIgnoresAProxyFromTheEnvironment(t *testing.T) {
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Store(true)
		fmt.Fprint(w, "via proxy")
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "direct")
	}))
	res := run(t, tool, siteURL(srv, "/"))
	if proxied.Load() || !strings.Contains(res.Content, "direct") {
		t.Fatalf("went through the proxy: %q", res.Content)
	}
}
