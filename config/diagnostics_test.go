package config

import (
	"strings"
	"testing"
)

// A workspace file, trusted or not, may only make tools.diagnostics
// stricter and its bounds lower; the operator's file sets them.
func TestDiagnosticsAreTheOperators(t *testing.T) {
	for _, trust := range []TrustChoice{TrustRefused, TrustGranted} {
		_, ws := trustHome(t, `{"tools":{"diagnostics_max":10}}`, `{"tools":{"diagnostics":"on","diagnostics_max":100}}`)
		cfg, err := LoadWith(ws, LoadOptions{Trust: trust, Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		// A trusted workspace may raise the bound; it never turns diagnostics on.
		if cfg.Tools.Diagnostics == "on" || trust == TrustRefused && cfg.Tools.DiagnosticsMax != 10 {
			t.Fatalf("trust %v: diagnostics %q max %d", trust, cfg.Tools.Diagnostics, cfg.Tools.DiagnosticsMax)
		}
		_, ws = trustHome(t, `{"tools":{"diagnostics":"on"}}`, `{"tools":{"diagnostics":"off","diagnostics_wait_ms":500}}`)
		if cfg, _ := LoadWith(ws, LoadOptions{Trust: trust, Quiet: true}); cfg.Tools.Diagnostics != "off" || cfg.Tools.DiagnosticsWaitMS != 500 {
			t.Fatalf("trust %v: a stricter setting was not taken: %q %d", trust, cfg.Tools.Diagnostics, cfg.Tools.DiagnosticsWaitMS)
		}
	}
	_, ws := trustHome(t, `{"tools":{"diagnostics":"on","diagnostics_max":50}}`, "")
	if cfg, _ := LoadWith(ws, LoadOptions{Quiet: true}); cfg.Tools.Diagnostics != "on" || cfg.Tools.DiagnosticsMax != 50 {
		t.Fatalf("the operator's settings were not taken: %q %d", cfg.Tools.Diagnostics, cfg.Tools.DiagnosticsMax)
	}
}

func TestValidateDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		tools ToolsConfig
		ok    bool
	}{
		{ToolsConfig{Diagnostics: "on", DiagnosticsMax: 200, DiagnosticsWaitMS: 30000}, true},
		{ToolsConfig{Diagnostics: "off"}, true},
		{ToolsConfig{Diagnostics: "always"}, false},
		{ToolsConfig{DiagnosticsMax: 201}, false},
		{ToolsConfig{DiagnosticsWaitMS: -1}, false},
	} {
		cfg := Default()
		cfg.Tools = tc.tools
		err := cfg.Validate()
		if tc.ok != (err == nil) || err != nil && !strings.Contains(err.Error(), "tools.diagnostics") {
			t.Fatalf("%+v: %v", tc.tools, err)
		}
	}
}
