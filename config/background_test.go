package config

import (
	"strings"
	"testing"
	"time"
)

func TestServerBackgroundConfig(t *testing.T) {
	var b ServerBackgroundConfig
	if b.ApprovalTimeout() != 1740*time.Second || b.Running() != 2 {
		t.Fatalf("defaults: %v %d", b.ApprovalTimeout(), b.Running())
	}
	for _, tc := range []struct {
		b    ServerBackgroundConfig
		want string
	}{
		{ServerBackgroundConfig{ApprovalTimeoutSeconds: -1}, "approval_timeout_seconds"},
		{ServerBackgroundConfig{MaxRunning: -1}, "max_running"},
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
