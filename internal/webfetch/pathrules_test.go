package webfetch

import (
	"net/http"
	"strings"
	"testing"
)

// Variation selectors, the grapheme joiner and Hangul fillers are invisible
// too, and cannot pad a segment that reads as dots.
func TestMoreInvisibleCharactersBesideDotsAreRefused(t *testing.T) {
	for _, p := range []string{"/..%EF%B8%8F/x", "/%CD%8F../x", "/..%E3%85%A4/x", "/.%F3%A0%84%80./x", "/..%E1%85%9F/x"} {
		if _, err := (&Tool{}).check("https://example.com" + p); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
}

// An encoded slash is fetched, in one spelling, on a host no deny or ask rule
// names a path on, and refused where one does.
func TestEncodedSlashOnAHostWithNoPathRule(t *testing.T) {
	open := &Tool{PathRuled: PathRuled([]string{"web_fetch(https://gitlab.com/*)", "bash(rm *)"})}
	if _, err := open.check("https://gitlab.com/api/v4/projects/group%2Fproj"); err != nil {
		t.Errorf("group%%2Fproj: %v", err)
	}
	if _, err := open.check("https://registry.npmjs.org/@scope%2fname"); err == nil ||
		!strings.Contains(err.Error(), "write the URL as https://registry.npmjs.org/@scope%2Fname") {
		t.Errorf("a lower-case escape: %v", err)
	}
	if _, err := open.check("https://web.archive.org/web/2020/https:%2F%2Fexample.com%2Fdocs%2F"); err != nil {
		t.Errorf("a URL carried in the path: %v", err)
	}
	for _, p := range []string{"/a%2F..%2Fadmin", "/a%2F%2Fb", "/a%2F.%2Fb", "/x%2F%2E%2E", "/a%2F..%EF%B8%8F%2Fb", "/c:%2F..%2Fb", "/%2F%2Fevil"} {
		if _, err := open.check("https://example.com" + p); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
	for _, tc := range []struct {
		rule, host string
		ruled      bool
	}{
		{"web_fetch(https://gitlab.com/api/v4/projects/group/*)", "gitlab.com", true},
		{"web_fetch(https://gitlab.com/api/v4/projects/group/*)", "example.com", false},
		{"Web_Fetch(https://gitlab.com:443/admin*)", "gitlab.com", true},
		{"web_fetch(http*://*.gitlab.com/admin*)", "a.gitlab.com", true},
		{"web_fetch(*admin*)", "example.com", true},
		{"*(https://example.com/admin)", "example.com", true},
		{"web_fetch(https://example.com/**)", "example.com", false},
		{"web_fetch", "example.com", false},
	} {
		if got := PathRuled([]string{tc.rule})(tc.host); got != tc.ruled {
			t.Errorf("%s on %s: %v", tc.rule, tc.host, got)
		}
	}
	ruled := &Tool{PathRuled: PathRuled([]string{"web_fetch(https://gitlab.com/api/v4/projects/group/*)"})}
	if _, err := ruled.check("https://gitlab.com/api/v4/projects/group%2Fproj"); err == nil || !strings.Contains(err.Error(), "encoded slash") {
		t.Errorf("a host with a path rule: %v", err)
	}
}

// The encoded slash reaches the server as written.
func TestEncodedSlashIsSentAsWritten(t *testing.T) {
	got := make(chan string, 1)
	tool, srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.RequestURI
		_, _ = w.Write([]byte("ok"))
	}))
	tool.PathRuled = PathRuled(nil)
	if res := run(t, tool, siteURL(srv, "/projects/group%2Fproj")); res.IsError {
		t.Fatalf("not fetched: %s", res.Content)
	}
	if uri := <-got; uri != "/projects/group%2Fproj" {
		t.Fatalf("the server got %s", uri)
	}
}
