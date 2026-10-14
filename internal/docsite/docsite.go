// Package docsite serves the Abhed documentation from inside the binary.
//
// A public copy of the same pages may be published on the web, and for most
// deployments that is the convenient place to read them. It is the wrong one
// for the deployment this product is actually built for: an air-gapped
// install has no route out, and telling an operator in an enclave to "check
// the website" is telling them nothing.
//
// So the same generated HTML ships inside the binary. One generator, two
// outputs: whatever site an edition publishes, and this.
package docsite

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The generator writes here before a build. The directory is committed with a
// placeholder so the package compiles on a checkout that has never run it —
// a missing embed is a compile error, and failing to build because the docs
// have not been generated would be a hostile default.
//
//go:embed all:site
var site embed.FS

// PublicURL is where an edition also publishes these pages, if anywhere.
// Empty by default, and the default is the point: nothing in the binary
// links out unless whoever built it said where to. It is never rendered into
// a page — the console's air-gap guard forbids an external URL there — only
// printed at startup for the operator, when the build carries no embedded
// copy of its own.
var PublicURL string

// Available reports whether real documentation was embedded, as opposed to the
// placeholder. The server uses it to decide whether to advertise /docs at all;
// a link to an empty page is worse than no link.
func Available() bool {
	f, err := site.Open("site/guide/01-getting-started.html")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	return true
}

// CSP is the policy on every docs response. The pages run no script and load
// nothing; their one stylesheet is inline, so only inline style is allowed.
const CSP = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the embedded documentation under the given prefix.
func Handler(prefix string) http.Handler {
	sub, err := fs.Sub(site, "site")
	if err != nil {
		// Only reachable if the embed directive above stops matching, which
		// is a build-time mistake rather than a runtime condition.
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(sub))

	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", CSP)
		p := strings.TrimPrefix(r.URL.Path, "/")

		// The site is generated with extensionless links, because Cloudflare
		// Pages serves them that way. net/http does not, so the extension is
		// put back here rather than generating two different link styles for
		// two different servers.
		if p != "" && !strings.HasSuffix(p, "/") && !strings.Contains(path.Base(p), ".") {
			if _, err := fs.Stat(sub, p+".html"); err == nil {
				r2 := *r
				u := *r.URL
				u.Path = "/" + p + ".html"
				r2.URL = &u
				files.ServeHTTP(w, &r2)
				return
			}
		}

		// Documentation is static and public; it carries no session data, so
		// it is cacheable. Short, because a running binary is the thing being
		// documented and the two should not drift for long.
		w.Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(w, r)
	}))
}
