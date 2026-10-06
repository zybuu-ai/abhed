package egress

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
)

// FuzzParseRequestLine checks that whatever parses is one plain spelling:
// the host is canonical, the port is in range, and a plain request's path
// has no segment a server would rewrite.
func FuzzParseRequestLine(f *testing.F) {
	for _, s := range []string{
		"CONNECT example.com:443 HTTP/1.1", "GET http://example.com/a?b HTTP/1.1",
		"CONNECT [::1]:443 HTTP/1.1", "GET http://a.example.com:8080/x/../y HTTP/1.0",
		"GET http://EXAMPLE.com./%2e%2e/ HTTP/1.1", "CONNECT 0x7f.1:80 HTTP/1.1",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		tg, err := ParseRequestLine(line)
		if err != nil {
			return
		}
		checkTarget(t, tg)
		// The same target through net/http's own request reader.
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(line + "\r\nHost: x\r\n\r\n")))
		if err == nil {
			if tg2, err := ParseTarget(req.Method, req.RequestURI); err == nil && (tg2.Host != tg.Host || tg2.Port != tg.Port) {
				t.Fatalf("two parses disagree: %+v %+v", tg, tg2)
			}
		}
	})
}

// FuzzHostMatching checks the wildcard never matches a host that does not
// end in the rule's name on a label boundary.
func FuzzHostMatching(f *testing.F) {
	f.Add("example.com", "a.example.com")
	f.Add("example.com", "example.com.evil.net")
	f.Add("example.com", "evilexample.com")
	f.Fuzz(func(t *testing.T, base, host string) {
		b, err := CanonicalHost(base)
		if err != nil || !strings.Contains(b, ".") {
			return
		}
		h, err := CanonicalHost(host)
		if err != nil {
			return
		}
		if c, _ := CanonicalHost(h); c != h {
			t.Fatalf("CanonicalHost is not idempotent: %q -> %q", h, c)
		}
		got := HostMatches("."+b, true, h)
		want := strings.HasSuffix(h, "."+b)
		if got != want || (got && h == b) {
			t.Fatalf("*.%s vs %s: %v", b, h, got)
		}
	})
}

func checkTarget(t *testing.T, tg Target) {
	t.Helper()
	if c, err := CanonicalHost(tg.Host); err != nil || c != tg.Host {
		t.Fatalf("host %q is not canonical", tg.Host)
	}
	if tg.Port == 0 {
		t.Fatal("port 0")
	}
	if tg.Tunnel {
		return
	}
	if !strings.HasPrefix(tg.Path, "/") || strings.Contains(tg.Path, "/../") || strings.HasSuffix(tg.Path, "/..") ||
		strings.Contains(tg.Path, "//") || strings.Contains(tg.Path, "\\") {
		t.Fatalf("path %q", tg.Path)
	}
	if tg.URL == nil || tg.URL.User != nil || tg.URL.Scheme != "http" {
		t.Fatalf("url %v", tg.URL)
	}
}
