package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestServerMetricsConfig(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"server":{"metrics":{"enabled":true,"listen":"127.0.0.1:9464"}}}`), &c); err != nil {
		t.Fatal(err)
	}
	if !c.Server.Metrics.Enabled || c.Server.Metrics.Listen != "127.0.0.1:9464" {
		t.Fatalf("%+v", c.Server.Metrics)
	}
	if Default().Server.Metrics.Enabled {
		t.Fatal("metrics are on by default")
	}
	for _, c := range []struct {
		m    ServerMetricsConfig
		want string
	}{
		{ServerMetricsConfig{Enabled: true}, ""},
		{ServerMetricsConfig{Enabled: true, Listen: "127.0.0.1:9464"}, ""},
		{ServerMetricsConfig{Enabled: true, Listen: "[::1]:9464"}, ""},
		{ServerMetricsConfig{Enabled: true, Listen: "localhost:9464"}, ""},
		{ServerMetricsConfig{Enabled: true, Listen: ":9464"}, "reachable from other machines"},
		{ServerMetricsConfig{Enabled: true, Listen: "0.0.0.0:9464"}, "reachable from other machines"},
		{ServerMetricsConfig{Enabled: true, Listen: "0.0.0.0:9464", TokenEnv: "T"}, ""},
		{ServerMetricsConfig{Enabled: true, Listen: "nope"}, "host:port"},
		{ServerMetricsConfig{Enabled: true, TokenEnv: "T"}, "listen, which is unset"},
	} {
		err := c.m.validate()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%+v: %v, want %q", c.m, err, c.want)
		}
	}
	t.Setenv("ABHED_TEST_METRICS_TOKEN", "t0ken")
	if (ServerMetricsConfig{TokenEnv: "ABHED_TEST_METRICS_TOKEN"}).MetricsToken() != "t0ken" {
		t.Fatal("token not read")
	}
}
