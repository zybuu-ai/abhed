package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A key no setting reads still loads, as it did, but is named with its
// path and, where one is close, the key that was probably meant.
func TestUnknownKeysAreReportedNotRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	path := filepath.Join(ws, ".abhed", "config.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(`{
	  "model": {"provider": "custom", "providers": {"custom": {
	    "type": "openai-compatible", "base_url": "http://gpu:8000/v1", "model": "m", "contxt_window": 8192}}},
	  "sandbox": {"allow_networks": true, "Min_Tier": "process"},
	  "permissions": {"deny": ["bash(rm*)"]},
	  "extensions": [{"name": "x", "command": "/bin/x", "evnets": ["tool_call"]}],
	  "zzz_nothing_like_it": 1,
	  "_comment": "annotations are for people", "$schema": "./schema.json",
	  "storage": {"_note": "memory for now"}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var warned bytes.Buffer
	warnOut = &warned
	t.Cleanup(func() { warnOut = os.Stderr })

	cfg, err := Load(ws)
	if err != nil && !strings.Contains(err.Error(), "model") {
		t.Fatalf("a config with unknown keys stopped loading: %v", err)
	}
	got := map[string]string{}
	for _, u := range cfg.Unknown {
		if u.File != path {
			t.Errorf("%s: file %q, want %q", u.Path, u.File, path)
		}
		got[u.Path] = u.Suggest
	}
	want := map[string]string{
		"model.provider":                       "model.default",
		"model.providers.custom.contxt_window": "model.providers.custom.context_window",
		"sandbox.allow_networks":               "sandbox.allow_network",
		"extensions[0].evnets":                 "extensions[0].events",
		"zzz_nothing_like_it":                  "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unknown keys = %v\nwant %v", got, want)
	}
	if cfg.Sandbox.AllowNetwork {
		t.Error("a misspelt key took effect")
	}
	if cfg.Sandbox.MinTier != "process" {
		t.Error("a key in another case is read by the decoder and must not be reported")
	}

	// Once per key, however often the file is loaded.
	_, _ = Load(ws)
	for p := range want {
		if n := strings.Count(warned.String(), "unknown key "+p+" "); n != 1 {
			t.Errorf("%s warned %d times:\n%s", p, n, warned.String())
		}
	}
	if !strings.Contains(warned.String(), "model.provider is ignored (did you mean model.default?)") {
		t.Errorf("the warning does not say what was meant:\n%s", warned.String())
	}
}

// A key in the managed file says who has to correct it.
func TestUnknownKeyInTheManagedFileSaysSo(t *testing.T) {
	u := UnknownKey{File: "/etc/abhed/config.json", Path: "model.provider", Suggest: "model.default", Managed: true}
	if !strings.Contains(u.String(), "managed configuration") {
		t.Fatalf("the managed file is not named as such: %s", u)
	}
	if strings.Contains((UnknownKey{File: "a", Path: "b"}).String(), "managed") {
		t.Fatal("a workspace file is called managed")
	}
}

// The shipped example uses only keys that are read.
func TestExampleConfigHasNoUnknownKeys(t *testing.T) {
	data, err := os.ReadFile("../config.example.json")
	if err != nil {
		t.Skip(err)
	}
	if u := unknownKeys("config.example.json", data, reflect.TypeFor[Config]()); len(u) > 0 {
		t.Fatalf("config.example.json has keys nothing reads: %v", u)
	}
}
