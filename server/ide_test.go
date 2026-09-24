package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The page draws file contents, tool output and session titles, all of which
// are untrusted. It may never build markup from a string, and it may never
// load anything from another host: it has to open on an air-gapped network.
func TestIDEPageNeverAssemblesMarkupOrLoadsRemotely(t *testing.T) {
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(ideHTML, bad) {
			t.Errorf("the IDE page uses %s; untrusted text must go in as textContent", bad)
		}
	}
	if m := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//`).FindString(ideHTML); m != "" {
		t.Errorf("the IDE page references a remote resource: %q", m)
	}
	if strings.Contains(ideHTML, "@import") || regexp.MustCompile(`url\(\s*["']?https?:`).MatchString(ideHTML) {
		t.Error("the IDE page pulls a remote stylesheet or font")
	}
}

// Replies are rendered as markdown, and a reply can quote anything the agent
// read. The renderer builds nodes from text, and links only to web addresses.
func TestIDEMarkdownIsBuiltFromText(t *testing.T) {
	harness := `import { El } from './dom.mjs';
globalThis.__root = new El('div');
`
	if out, err := runConsoleCases(t, "ide-md", harness, "ide_md_cases.mjs"); err != nil {
		t.Fatalf("the workbench's markdown renderer failed:\n%s", out)
	}
}

func TestIDEIsServedUnderTheConsolesPolicy(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/ide", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ide = %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'", "font-src 'self';"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q is missing %q", csp, want)
		}
	}
	// The editor's workers are same-origin files, so neither blob: nor worker-src is needed.
	for _, loose := range []string{"unsafe-eval", "http", "blob:", "worker-src", "*"} {
		if strings.Contains(csp, loose) {
			t.Errorf("CSP was loosened with %q: %q", loose, csp)
		}
	}
}

// The Tools and Extensions panels show what the agent can really reach. An
// extension's command and environment are the operator's, and may hold
// credentials, so only its name and events leave the server.
func TestCapabilitiesReportsToolsAndWithholdsExtensionSecrets(t *testing.T) {
	cfg := config.Default()
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "read(**/.env)")
	cfg.Extensions = []config.ExtensionConfig{{
		Name: "scanner", Command: "/opt/scan --token=sk-live-123", Events: []string{"tool_call"},
		Env: map[string]string{"SCANNER_KEY": "hunter2"},
	}}
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}, tools.Write{}, tools.Bash{})})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/capabilities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, leak := range []string{"sk-live-123", "hunter2", "/opt/scan", "SCANNER_KEY"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("capabilities leaked %q from an extension's configuration", leak)
		}
	}
	var c capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	mutates := map[string]bool{}
	for _, tool := range c.Tools {
		mutates[tool.Name] = tool.Mutates
	}
	if mutates["read"] || !mutates["write"] || !mutates["bash"] {
		t.Errorf("mutation flags are wrong: %v", mutates)
	}
	if _, ok := mutates["recall"]; !ok {
		t.Error("recall is missing: every session has it, though the shared registry does not")
	}
	if len(c.Extensions) != 1 || c.Extensions[0].Name != "scanner" || c.Extensions[0].Events[0] != "tool_call" {
		t.Errorf("extensions = %+v", c.Extensions)
	}
	if !strings.Contains(strings.Join(c.Permissions.Deny, " "), "read(**/.env)") {
		t.Errorf("deny rules = %v", c.Permissions.Deny)
	}
	// Empty lists must be [] and not null, or the page throws on .length.
	if !strings.Contains(rec.Body.String(), `"mcp":[]`) || !strings.Contains(rec.Body.String(), `"skills":[]`) {
		t.Errorf("empty lists are not arrays: %s", rec.Body)
	}
}

// An MCP tool is attributed to its server, which is how the panel groups them.
func TestCapabilitiesAttributesMCPTools(t *testing.T) {
	if got := firstSentence("Read a file. It must exist. More detail follows here."); got != "Read a file." {
		t.Errorf("firstSentence = %q", got)
	}
	long := strings.Repeat("अभेद ", 80)
	if got := firstSentence(long); !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Errorf("a long description was cut inside a character: %q", got)
	}
}

// Neither the page nor what it reads is public. With sign-in on, an anonymous
// caller must not learn which tools, rules and extensions this server has.
// /console is the control: it is known to be protected, so if it answered 200
// here the fixture would be enforcing nothing and the test proving nothing.
func TestIDEAndCapabilitiesNeedSignIn(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	s := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}),
		Auth:     &auth.Middleware{Providers: []auth.Provider{local}, PublicPaths: []string{"/", "/login"}}})
	for _, path := range []string{"/console", "/ide", "/v1/capabilities"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s answered 200 to an anonymous caller", path)
		}
	}
}

// The vendored components come from the binary and nowhere else.
func TestIDEVendorServesOnlyEmbeddedFiles(t *testing.T) {
	h := testServer(t).Handler()
	for path, want := range map[string]int{
		"/ide/vendor/editor.js": http.StatusOK, "/ide/vendor/editor.css": http.StatusOK,
		"/ide/vendor/codicon.ttf": http.StatusOK, "/ide/vendor/editor.worker.js": http.StatusOK,
		"/ide/vendor/json.worker.js": http.StatusOK, "/ide/vendor/xterm.css": http.StatusOK,
		"/ide/vendor/NOTICE": http.StatusOK, "/ide/vendor/missing.js": http.StatusNotFound,
		"/ide/vendor/..%2Fide.html": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s = %d, want %d", path, rec.Code, want)
		}
		if rec.Code == http.StatusOK && rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s served without nosniff", path)
		}
	}
}

// Components go out gzipped to a client that takes it and plain to one that
// does not, the same bytes either way, each encoding with its own ETag.
func TestIDEVendorServesBothEncodings(t *testing.T) {
	h := testServer(t).Handler()
	get := func(enc, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/ide/vendor/editor.worker.js", nil)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	zipped, plain, refused := get("br, gzip;q=0.8", ""), get("", ""), get("gzip;q=0", "")
	if zipped.Header().Get("Content-Encoding") != "gzip" || plain.Header().Get("Content-Encoding") != "" || refused.Header().Get("Content-Encoding") != "" {
		t.Fatalf("encodings: %q %q %q", zipped.Header().Get("Content-Encoding"), plain.Header().Get("Content-Encoding"), refused.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	unzipped, _ := io.ReadAll(zr)
	if !bytes.Equal(unzipped, plain.Body.Bytes()) || len(unzipped) == 0 {
		t.Fatal("the two encodings do not carry the same file")
	}
	for _, rec := range []*httptest.ResponseRecorder{zipped, plain} {
		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("Vary = %q", rec.Header().Get("Vary"))
		}
		// A worker takes its policy from its own response.
		if csp := rec.Header().Get("Content-Security-Policy"); csp != "default-src 'none'; script-src 'self'" {
			t.Errorf("worker CSP = %q", csp)
		}
	}
	zTag, pTag := zipped.Header().Get("ETag"), plain.Header().Get("ETag")
	if zTag == "" || zTag == pTag {
		t.Fatalf("ETags must differ per encoding: %q %q", zTag, pTag)
	}
	if get("gzip", zTag).Code != http.StatusNotModified || get("gzip", pTag).Code != http.StatusOK {
		t.Fatal("revalidation does not follow the encoding")
	}
}
