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
	// A redirect to the same URL is the one kind followed, so the second hop
	// dials the name again.
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
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

// A redirect to another path on the same host is handed back too: a rule on
// that path must see it. Only a redirect to the same URL is followed, up to
// the limit.
func TestOnlyARedirectToTheSameURLIsFollowed(t *testing.T) {
	var reachedNew atomic.Bool
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/admin/new", http.StatusMovedPermanently)
		case "/admin/new":
			reachedNew.Store(true)
		case "/creds":
			http.Redirect(w, r, "http://u:p@"+r.Host+"/creds", http.StatusFound)
		default:
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
		}
	}))
	res := run(t, tool, siteURL(srv, "/old"))
	if res.IsError || reachedNew.Load() || !strings.Contains(res.Content, "redirects to "+siteURL(srv, "/admin/new")) {
		t.Fatalf("a same-host redirect to another path must be handed back: %q", res.Content)
	}
	res = run(t, tool, siteURL(srv, "/creds"))
	if strings.Contains(res.Content, "u:p@") || !strings.Contains(res.Content, "not followed") {
		t.Fatalf("a redirect carrying user info must be handed back without it: %q", res.Content)
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

// judge runs one call as the loop does: canonical arguments, the policy
// decision on them, and the tool on the same arguments when allowed.
func judge(t *testing.T, e *policy.Engine, tool *Tool, rawURL string) (policy.Decision, tools.Result) {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"url": rawURL})
	canon, _, err := tools.CanonicalArgs(tool, args)
	if err != nil {
		t.Fatal(err)
	}
	d := e.Evaluate("web_fetch", false, canon).Decision
	if d != policy.Allow {
		return d, tools.Result{}
	}
	return d, tool.Run(context.Background(), nil, canon)
}

// Policy and the tool judge the same string: a URL padded with spaces or a
// line break is refused, not trimmed and fetched past a deny rule.
func TestAPaddedURLNeverGetsPastADenyRule(t *testing.T) {
	var reached atomic.Bool
	tool, srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	e := policy.New(policy.ModeBypass)
	if err := e.AddDeny("web_fetch(" + siteURL(srv, "/*") + ")"); err != nil {
		t.Fatal(err)
	}
	if d, _ := judge(t, e, tool, siteURL(srv, "/x")); d != policy.Deny {
		t.Fatalf("the plain URL: %s", d)
	}
	for _, u := range []string{" " + siteURL(srv, "/x"), siteURL(srv, "/x") + "\n", "\t" + siteURL(srv, "/x") + " "} {
		d, res := judge(t, e, tool, u)
		if d == policy.Allow && !res.IsError {
			t.Errorf("%q: fetched: %q", u, res.Content)
		}
	}
	if reached.Load() {
		t.Fatal("a padded URL reached the server")
	}
}

// A path with . or .. or an empty segment is refused, raw or encoded, so a
// rule on a path prefix cannot be dodged, nor an allow rule widened, and the
// refusal never offers a dotted spelling.
func TestDotSegmentsCannotDodgeOrWidenAPathRule(t *testing.T) {
	var hits atomic.Int32
	tool, srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	deny := policy.New(policy.ModeBypass)
	_ = deny.AddDeny("web_fetch(" + siteURL(srv, "/admin*") + ")")
	allow := policy.New(policy.ModeDefault)
	allow.AskReadOnly = AskReadOnly(true, nil)
	_ = allow.AddAllow("web_fetch(" + siteURL(srv, "/public/*") + ")")

	for _, p := range []string{
		"/public/../admin/x", "/./admin/x", "//admin/x", "/public/%2e%2e/admin/x",
		"/public/%2E%2E/admin/x", "/%2e/admin/x", "/public/..;/admin/x", "/public/%2f/admin",
		"/public\\..\\admin",
	} {
		for name, e := range map[string]*policy.Engine{"deny": deny, "allow": allow} {
			d, res := judge(t, e, tool, siteURL(srv, p))
			if d == policy.Allow && !res.IsError {
				t.Errorf("%s %s: fetched: %q", name, p, res.Content)
			}
		}
		_, err := tool.check(siteURL(srv, p))
		if err == nil {
			t.Errorf("%s: accepted", p)
		} else if strings.Contains(err.Error(), "/..") || strings.Contains(err.Error(), "/./") || strings.Contains(err.Error(), "write the URL as") {
			t.Errorf("%s: the refusal offers a spelling: %v", p, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the server was reached %d times", hits.Load())
	}
	// A trailing slash is ordinary.
	if _, err := tool.check(siteURL(srv, "/public/docs/")); err != nil {
		t.Fatal(err)
	}
}

func TestMoreIPv6FormsAreRefused(t *testing.T) {
	for _, a := range []string{"::127.0.0.1", "::ffff:0:a00:1", "::", "2001:10::1", "3fff::1", "192.88.99.1"} {
		why := blockedAddr(netip.MustParseAddr(a))
		if why == "" {
			t.Errorf("%s allowed", a)
		}
	}
	if why := blockedAddr(netip.MustParseAddr("::")); why != "an unspecified address" {
		t.Errorf("%q", why)
	}
	if why := blockedAddr(netip.MustParseAddr("2606:4700::1111")); why != "" {
		t.Errorf("a public address refused: %s", why)
	}
}

// A value written in another case, such as into a host name, which is
// lower-cased on the way out, is still refused.
func TestASecretIsFoundWhateverItsCase(t *testing.T) {
	store := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := store.Set("TOKEN", "S3cr3tValue123"); err != nil {
		t.Fatal(err)
	}
	tool := &Tool{Secrets: store.LoadRedactor}
	if _, err := tool.check("https://s3cr3tvalue123.attacker.example/"); err == nil || !strings.Contains(err.Error(), "[secret:TOKEN]") {
		t.Fatalf("got %v", err)
	}
}

func TestHTMLTextWithAnUnclosedHeadAndAQuotedBracket(t *testing.T) {
	_, text := htmlText(`<html><head><title>T</title><body><p>Body text</p><a href="/x" title="a>b">link</a>`, nil)
	if !strings.Contains(text, "Body text") {
		t.Errorf("the body was lost:\n%s", text)
	}
	if strings.Contains(text, `b">`) {
		t.Errorf("attribute text leaked:\n%s", text)
	}
}

// A host and a port have one spelling each, so a rule on an address or a port
// cannot be stepped around by writing it another way.
func TestHostAndPortHaveOneSpelling(t *testing.T) {
	var reached atomic.Int32
	tool, srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	e := policy.New(policy.ModeBypass)
	_ = e.AddDeny("web_fetch(http://site.test:"+port+"/*)", "web_fetch(http://127.0.0.1:"+port+"/*)")
	for _, u := range []string{"http://site.test:" + port + "/x", "http://127.0.0.1:" + port + "/x"} {
		if d, _ := judge(t, e, tool, u); d != policy.Deny {
			t.Fatalf("%s: %s, want the rule to hold", u, d)
		}
	}
	for _, u := range []string{
		"http://site.test:0" + port + "/x",
		"http://site.test:00" + port + "/admin",
		"http://127.0.0.1:0" + port + "/x",
		"http://[::ffff:127.0.0.1]:" + port + "/x",
		"http://[::ffff:7f00:1]:" + port + "/x",
		"http://2130706433:" + port + "/x",
		"http://127.1:" + port + "/x",
		"http://0x7f.1:" + port + "/x",
		"http://0x7f000001:" + port + "/x",
		"http://127.0.0.01:" + port + "/x",
	} {
		d, res := judge(t, e, tool, u)
		if d == policy.Allow && !res.IsError {
			t.Errorf("%s: fetched past the rule: %q", u, res.Content)
		}
		if _, err := tool.check(u); err == nil {
			t.Errorf("%s: accepted", u)
		}
	}
	if reached.Load() != 0 {
		t.Fatalf("the server was reached %d times", reached.Load())
	}

	plain := &Tool{}
	for u, want := range map[string]string{
		"https://example.com:0443/":         "write the URL as https://example.com/",
		"https://example.com:08443/a":       "write the URL as https://example.com:8443/a",
		"https://example.com:0/":            "1 to 65535",
		"https://example.com:65536/":        "1 to 65535",
		"http://[::ffff:93.184.216.34]/":    "write the address as 93.184.216.34",
		"http://[2606:4700:0:0::1111]/":     "write the URL as http://[2606:4700::1111]/",
		"http://1572395042/":                "four decimal numbers",
		"http://93.184.55330/":              "four decimal numbers",
		"http://example.com.123/":           "four decimal numbers",
		"http://93.184.216.34/":             "ok",
		"http://[2606:4700::1111]:8080/a?b": "ok",
	} {
		_, err := plain.check(u)
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

// Segments a server that trims or decodes twice would read as .. or / are
// refused too.
func TestLookAlikeDotSegmentsAreRefused(t *testing.T) {
	for _, p := range []string{"/%252e%252e/admin", "/..%00/admin", "/..%09/admin", "/%20../admin",
		"/.../admin", "/a%252fb", "/a%255cb", "/ok/%2e%20/x"} {
		if _, err := (&Tool{}).check("https://example.com" + p); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
	if _, err := (&Tool{}).check("https://example.com/v1.2/file.tar.gz"); err != nil {
		t.Errorf("an ordinary dotted name refused: %v", err)
	}
}

// One deny rule written with http* covers both schemes, as the docs advise.
func TestADenyRuleCanCoverBothSchemes(t *testing.T) {
	var reached atomic.Int32
	tool, srv := serve(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	e := policy.New(policy.ModeBypass)
	_ = e.AddDeny("web_fetch(http*://site.test:" + port + "/*)")
	for _, u := range []string{"http://site.test:" + port + "/x", "https://site.test:" + port + "/x"} {
		if d, _ := judge(t, e, tool, u); d != policy.Deny {
			t.Errorf("%s: %s", u, d)
		}
	}
	if d, _ := judge(t, e, tool, "http://other.test/"); d != policy.Allow {
		t.Errorf("another host: %s", d)
	}
	if reached.Load() != 0 {
		t.Fatal("the server was reached")
	}
}

// With a host list, a listed host runs unasked on its scheme's default port
// only; another port asks, in plan mode too, unless an allow rule names it.
func TestAHostListRunsUnaskedOnlyOnTheDefaultPort(t *testing.T) {
	ask := AskReadOnly(true, []string{"docs.example.com"})
	for _, tc := range []struct {
		mode  policy.Mode
		url   string
		allow string
		want  policy.Decision
	}{
		{policy.ModeDefault, "https://docs.example.com/a", "", policy.Allow},
		{policy.ModeDefault, "http://docs.example.com/a", "", policy.Allow},
		{policy.ModeDefault, "https://docs.example.com:8443/a", "", policy.Ask},
		{policy.ModeAuto, "http://docs.example.com:6379/", "", policy.Ask},
		{policy.ModePlan, "http://docs.example.com:25/", "", policy.Ask},
		{policy.ModeDefault, "https://docs.example.com:8443/a", "web_fetch(https://docs.example.com:8443/*)", policy.Allow},
		{policy.ModeBypass, "https://docs.example.com:8443/a", "", policy.Allow},
	} {
		e := policy.New(tc.mode)
		e.AskReadOnly = ask
		if tc.allow != "" {
			_ = e.AddAllow(tc.allow)
		}
		args, _ := json.Marshal(map[string]string{"url": tc.url})
		got := e.Evaluate("web_fetch", false, args)
		if got.Decision != tc.want {
			t.Errorf("%s %s: %s (%s), want %s", tc.mode, tc.url, got.Decision, got.Reason, tc.want)
		}
		if got.Decision == policy.Ask && got.Reason != PortAskReason {
			t.Errorf("%s: reason %q", tc.url, got.Reason)
		}
	}
	if AskReadOnly(false, nil) != nil {
		t.Error("a disabled tool set an ask")
	}
}

// Round 3: segments that read as dots once Unicode-normalised, encoded
// slashes with no spelling offered, and an empty host label named as such.
func TestUnicodeDotsEncodedSlashesAndEmptyLabels(t *testing.T) {
	for _, p := range []string{"/%EF%BC%8E%EF%BC%8E/admin", "/%C2%A0../x", "/..%C2%A0/x", "/%EF%BC%8F/x"} {
		if _, err := (&Tool{}).check("https://example.com" + p); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
	for _, u := range []string{"https://gitlab.com/api/v4/projects/group%2Fproj", "https://registry.npmjs.org/@scope%2fname"} {
		_, err := (&Tool{}).check(u)
		if err == nil || !strings.Contains(err.Error(), "encoded slash") || strings.Contains(err.Error(), "write the URL as") {
			t.Errorf("%s: %v", u, err)
		}
	}
	_, err := (&Tool{}).check("https://example.com../")
	if err == nil || !strings.Contains(err.Error(), "empty part") {
		t.Errorf("example.com..: %v", err)
	}
	if _, err := (&Tool{}).check("https://example.com/caf%C3%A9/.well-known/x"); err != nil {
		t.Errorf("an ordinary path refused: %v", err)
	}
}
