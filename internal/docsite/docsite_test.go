package docsite

import (
	"io/fs"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// The embedded docs are read where there is no route out, so they may not
// load anything from another host. A canonical link or an anchor loads nothing.
func TestEmbeddedDocsLoadNothingFromOutside(t *testing.T) {
	if !Available() {
		t.Skip("no documentation embedded in this build; run scripts/docsite/build.py --embed-only")
	}
	// Either quote style: the generator uses double quotes, a hand edit may not.
	loads := regexp.MustCompile(`(?i)<link[^>]+rel=["']?(stylesheet|preconnect|preload|icon|dns-prefetch)["']?[^>]+href=["']?https?://|` +
		`<link[^>]+href=["']?https?://[^"'>]*["']?[^>]+rel=["']?(stylesheet|preconnect|preload|icon|dns-prefetch)|` +
		`<(script|img|iframe)[^>]+src=["']?https?://|@import|url\(\s*["']?https?://`)
	pages := 0
	err := fs.WalkDir(site, "site", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		b, err := site.ReadFile(p)
		if err != nil {
			return err
		}
		pages++
		if m := loads.FindString(string(b)); m != "" {
			t.Errorf("%s loads from outside: %q", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages == 0 {
		t.Fatal("documentation is embedded but no page was checked")
	}
}

// Every docs response carries a policy that runs no script, embedded or not.
func TestHandlerSetsCSP(t *testing.T) {
	for _, p := range []string{"/docs/", "/docs/guide/01-getting-started", "/docs/missing.html"} {
		rec := httptest.NewRecorder()
		Handler("/docs").ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
			t.Errorf("%s: CSP %q", p, csp)
		}
	}
}
