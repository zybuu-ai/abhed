package secrets

import (
	"path/filepath"
	"strings"
	"testing"
)

// A value added after a session's redactor was made is redacted from then on.
func TestAlsoRedactReachesAFreshRedactor(t *testing.T) {
	t.Cleanup(func() { extra.Lock(); extra.m = nil; extra.gen++; extra.Unlock() })
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	f := s.Session()
	const v = "sk-model-key-extra-0123456789"
	if strings.Contains(string(f.Redact([]byte(`"`+v+`"`))), "[secret:") {
		t.Fatal("redacted before it was known")
	}
	AlsoRedact("TEST_EXTRA_KEY", v)
	AlsoRedact("TEST_EXTRA_KEY", "sk-model-key-other-0123456789")
	out := string(f.Redact([]byte(`{"a":"` + v + `","b":"sk-model-key-other-0123456789"}`)))
	if strings.Contains(out, "sk-model-key") {
		t.Fatalf("not redacted: %s", out)
	}
	AlsoRedact("SHORT", "abc")
	if string(f.Redact([]byte(`"abc"`))) != `"abc"` {
		t.Fatal("a short value was redacted in ordinary text")
	}
}

func TestModelKeyNamesAreHeld(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := s.Set(ModelKeyPrefix+"ANTHROPIC_API_KEY", "value-0123456789"); err == nil {
		t.Fatal("a person set a model key name with abhed secret set")
	}
}
