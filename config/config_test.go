package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("shipped default must be usable: %v", err)
	}
}

func TestProjectConfigOverridesUser(t *testing.T) {
	ws := t.TempDir()
	_ = os.MkdirAll(filepath.Join(ws, ".abhed"), 0o755)
	os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(`{
      "model":{"default":"custom","providers":{"custom":{
        "type":"openai-compatible","base_url":"http://gpu:8000/v1",
        "model":"qwen3-32b","context_window":131072}}}}`), 0o644)

	cfg, err := Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	p, err := cfg.Provider()
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "qwen3-32b" || p.BaseURL != "http://gpu:8000/v1" {
		t.Fatalf("project config not applied: %+v", p)
	}
	// Fields absent from the file keep their defaults.
	if cfg.Limits.MaxTurns != 100 {
		t.Fatalf("merge should preserve unset defaults, got %d", cfg.Limits.MaxTurns)
	}
}

func TestEnvOverridesEndpoint(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("ABHED_BASE_URL", "http://from-env:9000/v1")
	t.Setenv("ABHED_MODEL", "env-model")

	cfg, err := Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Provider()
	if p.BaseURL != "http://from-env:9000/v1" || p.Model != "env-model" {
		t.Fatalf("env override failed: %+v", p)
	}
}

func TestAPIKeyFromEnvVar(t *testing.T) {
	t.Setenv("MY_KEY", "secret-value")
	c := Default()
	p := c.Model.Providers["local"]
	p.APIKeyEnv = "MY_KEY"
	c.Model.Providers["local"] = p

	got, err := c.Provider()
	if err != nil {
		t.Fatal(err)
	}
	if got.APIKey != "secret-value" {
		t.Fatalf("api key not resolved from env, got %q", got.APIKey)
	}
}

func TestUnknownProviderNamesAlternatives(t *testing.T) {
	c := Default()
	c.Model.Default = "nope"
	_, err := c.Provider()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Available") {
		t.Fatalf("error should list valid providers: %v", err)
	}
}

func TestInvalidModeRejected(t *testing.T) {
	c := Default()
	c.Permissions.Mode = "yolo"
	if err := c.Validate(); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}

func TestCompactAtBounds(t *testing.T) {
	for _, v := range []float64{0, -1, 1.5} {
		c := Default()
		c.Context.CompactAt = v
		if err := c.Validate(); err == nil {
			t.Errorf("compact_at %v must be rejected", v)
		}
	}
}

func TestWriteDefaultRoundTrips(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, ".abhed", "config.json")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(ws)
	if err != nil {
		t.Fatalf("written default must load cleanly: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("written default must validate: %v", err)
	}
}

// The drain budget has to survive a round trip through the config file, or a
// deployment sets it and the node still ends turns at once.
func TestDrainSecondsRoundTrips(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"server":{"drain_seconds":45}}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Server.DrainSeconds != 45 {
		t.Fatalf("drain_seconds = %d, want 45", c.Server.DrainSeconds)
	}

	// Absent means zero, which is the documented "end turns at once".
	var d Config
	if err := json.Unmarshal([]byte(`{"server":{}}`), &d); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Server.DrainSeconds != 0 {
		t.Fatalf("an unset drain_seconds became %d", d.Server.DrainSeconds)
	}
}

// Unset and zero mean different things for offload_at: the default, and off.
func TestOffloadAtDistinguishesUnsetFromOff(t *testing.T) {
	var unset, off, custom Config
	_ = json.Unmarshal([]byte(`{"context":{"compact_at":0.9}}`), &unset)
	_ = json.Unmarshal([]byte(`{"context":{"compact_at":0.9,"offload_at":0}}`), &off)
	_ = json.Unmarshal([]byte(`{"context":{"compact_at":0.9,"offload_at":0.45}}`), &custom)
	if got := unset.Context.OffloadFraction(); got != 0.60 {
		t.Errorf("unset = %v, want the 0.60 default", got)
	}
	if got := off.Context.OffloadFraction(); got != 0 {
		t.Errorf("explicit 0 = %v, want off", got)
	}
	if got := custom.Context.OffloadFraction(); got != 0.45 {
		t.Errorf("custom = %v, want 0.45", got)
	}

	bad := Default()
	v := 0.95
	bad.Context.OffloadAt = &v
	if err := bad.Validate(); err == nil {
		t.Error("offload_at above compact_at was accepted: compaction would always run first")
	}
}
