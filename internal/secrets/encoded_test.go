package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A value of 12 or more characters is redacted and found in the forms a
// command or client encodes it in, at any base64 alignment.
func TestEncodedFormsAreRedactedAndFound(t *testing.T) {
	const v = "s3cr3t-Value-0123456789"
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := s.Set("TOKEN", v); err != nil {
		t.Fatal(err)
	}
	r := s.Redactor()
	forms := map[string]string{
		"basic":     "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+v)),
		"align1":    base64.StdEncoding.EncodeToString([]byte("x" + v + "y")),
		"align2":    base64.StdEncoding.EncodeToString([]byte("xy" + v)),
		"base64url": base64.URLEncoding.EncodeToString([]byte("?" + v)),
		"hex":       hex.EncodeToString([]byte(v)),
		"HEX":       strings.ToUpper(hex.EncodeToString([]byte(v))),
		"query":     "q=" + url.QueryEscape(v+" !"),
		"upper":     strings.ToUpper(v),
		"lower":     strings.ToLower(v),
	}
	for name, text := range forms {
		payload, _ := json.Marshal(map[string]string{"out": text})
		got := string(r.Redact(payload))
		if !strings.Contains(got, "[secret:TOKEN]") {
			t.Errorf("%s: not redacted: %s", name, got)
		}
		if _, found := r.FindSent(text); !found {
			t.Errorf("%s: FindSent missed %q", name, text)
		}
	}
	short := Open(filepath.Join(t.TempDir(), "secrets.json"))
	_ = short.Set("SHORT", "abcdefgh")
	enc := base64.StdEncoding.EncodeToString([]byte("abcdefgh"))
	if got := short.Redactor().Redact([]byte(`"` + enc + `"`)); string(got) != `"`+enc+`"` {
		t.Errorf("a value under 12 characters must not be matched encoded: %s", got)
	}
}

// One pass, longest first: a shorter value that spells part of a label never
// rewrites a label already written.
func TestRedactionNeverRewritesALabel(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	_ = s.Set("A", "aaaaaaaaaaaa")
	_ = s.Set("B", "secret:A")
	got := string(s.Redactor().Redact([]byte(`{"x":"aaaaaaaaaaaa then secret:A"}`)))
	if got != `{"x":"[secret:A] then [secret:B]"}` {
		t.Fatalf("got %s", got)
	}
}

// Plain text, which the control plane and the prompt history pass, is
// redacted as text rather than left alone for want of a JSON string.
func TestPlainTextIsRedacted(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	_ = s.Set("TOKEN", "ghp_0123456789abcdef")
	if got := string(s.Redactor().Redact([]byte("export T=ghp_0123456789abcdef; run"))); got != "export T=[secret:TOKEN]; run" {
		t.Fatalf("got %q", got)
	}
}

// A path's lock entry goes once no store on it is left.
func TestPathLocksShrink(t *testing.T) {
	dir := t.TempDir()
	before := lockedPaths()
	for i := range 50 {
		_ = Open(filepath.Join(dir, strings.Repeat("x", i+1)+".json"))
	}
	deadline := time.Now().Add(10 * time.Second)
	for lockedPaths() > before && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
	if n := lockedPaths(); n > before {
		t.Fatalf("%d lock entries left after every store went, want %d", n, before)
	}
	a, b := Open(filepath.Join(dir, "same.json")), Open(filepath.Join(dir, "same.json"))
	if a.mu != b.mu {
		t.Fatal("two stores on one path must share a lock")
	}
}

// A file path is matched against a value as written, never its encoded or
// other-case forms, which name ordinary files too often.
func TestFindInPathMatchesTheValueAsWritten(t *testing.T) {
	const v = "s3cr3t-Value-0123456789"
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := s.Set("TOKEN", v); err != nil {
		t.Fatal(err)
	}
	r := s.Redactor()
	if label, found := r.FindInPath("/tmp/" + v + "/x"); !found || !strings.Contains(label, "TOKEN") {
		t.Fatalf("the value in a path: %q %v", label, found)
	}
	for name, form := range map[string]string{
		"lower": strings.ToLower(v), "upper": strings.ToUpper(v),
		"hex": hex.EncodeToString([]byte(v)), "HEX": strings.ToUpper(hex.EncodeToString([]byte(v))),
	} {
		if _, found := r.FindInPath("/tmp/" + form + "/x"); found {
			t.Errorf("%s: an encoded form in a path was taken for the value", name)
		}
	}
}
