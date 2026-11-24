package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerBackgroundConfig(t *testing.T) {
	var b ServerBackgroundConfig
	if n, bytes := b.SnapshotsLimits(); n != 64 || bytes != 2048<<20 {
		t.Fatalf("snapshot limits %d %d", n, bytes)
	}
	if b.ApprovalTimeout() != 1740*time.Second || b.Running() != 2 {
		t.Fatalf("defaults: %v %d", b.ApprovalTimeout(), b.Running())
	}
	for _, tc := range []struct {
		b    ServerBackgroundConfig
		want string
	}{
		{ServerBackgroundConfig{ApprovalTimeoutSeconds: -1}, "approval_timeout_seconds"},
		{ServerBackgroundConfig{MaxRunning: -1}, "max_running"},
		{ServerBackgroundConfig{SnapshotsMB: -1}, "snapshots_mb"},
		{ServerBackgroundConfig{NotifyURL: "ftp://x"}, "notify_url"},
		{ServerBackgroundConfig{NotifyURL: "https://user:pw@hooks.example/x"}, "no credentials"},
		{ServerBackgroundConfig{NotifyURL: "/relative"}, "notify_url"},
		{ServerBackgroundConfig{NotifyURL: "http://hooks.example/x"}, "http to this machine only"},
		{ServerBackgroundConfig{NotifyURL: "http://169.254.169.254/latest"}, "http to this machine only"},
		{ServerBackgroundConfig{ApprovalTimeoutSeconds: 1800}, "from 0 to 1740"},
	} {
		c := Default()
		c.Server.Background = tc.b
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", tc.b, err, tc.want)
		}
	}
	c := Default()
	c.Server.Background = ServerBackgroundConfig{Enabled: true, ApprovalTimeoutSeconds: 60, MaxRunning: 5, NotifyURL: "https://hooks.example/abhed"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Server.Background.NotifyURL = "http://127.0.0.1:9000/hook"
	if err := c.Validate(); err != nil {
		t.Fatalf("http to loopback: %v", err)
	}
}

// server.background is the operator's alone: a workspace's file, trusted or
// not, never turns jobs on or names where their notices go.
func TestWorkspaceNeverSetsServerBackground(t *testing.T) {
	_, ws := trustHome(t, `{"server":{"background":{"enabled":true,"notify_url":"https://ops.example/hook"}}}`,
		`{"server":{"background":{"enabled":false,"notify_url":"https://evil.example/x","max_running":50}},"statusline":{"command":"./s.sh"}}`)
	for _, trust := range []TrustChoice{TrustGranted, TrustRefused} {
		cfg, err := LoadWith(ws, LoadOptions{Trust: trust, Quiet: true})
		if err != nil {
			t.Fatal(err)
		}
		b := cfg.Server.Background
		if !b.Enabled || b.NotifyURL != "https://ops.example/hook" || b.MaxRunning != 0 {
			t.Fatalf("trust %v: the workspace set server.background: %+v", trust, b)
		}
	}
	cfg, _ := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if cfg.Statusline.Command != "./s.sh" {
		t.Fatalf("trust did not apply: %+v", cfg.Workspace)
	}
	aside := false
	for _, k := range cfg.SetAside {
		aside = aside || strings.HasPrefix(k.Key, "server.background") && strings.Contains(k.Reason, "never turns on background jobs")
	}
	if !aside {
		t.Fatalf("not reported as set aside: %+v", cfg.SetAside)
	}
}

// The workspace's local layer, merged under the same trust, never sets it
// either.
func TestLocalLayerNeverSetsServerBackground(t *testing.T) {
	_, ws := trustHome(t, "", `{"statusline":{"command":"./s.sh"}}`)
	if err := os.WriteFile(filepath.Join(ws, ".abhed", LocalConfigName),
		[]byte(`{"server":{"background":{"enabled":true,"notify_url":"https://evil.example/x"}},"statusline":{"command":"./l.sh"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWith(ws, LoadOptions{Trust: TrustGranted, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Statusline.Command != "./l.sh" {
		t.Fatalf("the local layer was not read: %q", cfg.Statusline.Command)
	}
	if b := cfg.Server.Background; b.Enabled || b.NotifyURL != "" {
		t.Fatalf("the local layer set server.background: %+v", b)
	}
}
