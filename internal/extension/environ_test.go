package extension

import (
	"strings"
	"testing"
)

// An extension gets PATH, HOME and the locale from Abhed's environment, never
// Abhed's own keys, then its configured variables; an empty value passes
// Abhed's own value of that name. It inherited the whole environment, so
// ABHED_API_KEY and provider keys reached every extension.
func TestEnvironKeepsAbhedsKeysOut(t *testing.T) {
	t.Setenv("ABHED_API_KEY", "abhed-key-123456")
	t.Setenv("OPENAI_API_KEY", "sk-provider-123456")
	t.Setenv("HOME", "/home/someone")
	t.Setenv("EXT_TOKEN", "handed-on-purpose")

	got := environ(map[string]string{"FOO": "bar", "EXT_TOKEN": "", "HOME": "/override"})
	find := func(key string) (string, bool) {
		var val string
		var ok bool
		for _, kv := range got {
			if strings.HasPrefix(kv, key+"=") {
				val, ok = kv[len(key)+1:], true
			}
		}
		return val, ok
	}
	for _, k := range []string{"ABHED_API_KEY", "OPENAI_API_KEY"} {
		if _, ok := find(k); ok {
			t.Errorf("%s reached the extension", k)
		}
	}
	if v, _ := find("FOO"); v != "bar" {
		t.Errorf("FOO = %q", v)
	}
	if v, _ := find("EXT_TOKEN"); v != "handed-on-purpose" {
		t.Errorf("an empty value did not pass Abhed's own: %q", v)
	}
	if v, _ := find("HOME"); v != "/override" {
		t.Errorf("HOME = %q; a configured value wins", v)
	}
	if _, ok := find("PATH"); !ok {
		t.Error("PATH was dropped")
	}
	if environ(nil) == nil {
		t.Error("no configured variables must still give the base names, not Abhed's whole environment")
	}
}
