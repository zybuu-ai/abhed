package config

import (
	"strings"
	"testing"
	"time"
)

func TestServerBackgroundConfig(t *testing.T) {
	var b ServerBackgroundConfig
	if b.ApprovalTimeout() != time.Hour || b.Running() != 2 {
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
}
