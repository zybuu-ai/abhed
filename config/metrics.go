package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
)

// ServerMetricsConfig is server.metrics: Prometheus metrics of `abhed serve`.
type ServerMetricsConfig struct {
	// Enabled serves the metrics. Unset, nothing is counted or served.
	Enabled bool `json:"enabled,omitempty"`
	// Listen, when set, serves /metrics on this address alone, apart from the
	// API. Unset, /metrics is on the API's own address, for administrators.
	Listen string `json:"listen,omitempty"`
	// TokenEnv names an environment variable holding a bearer token the
	// separate listener asks for. Required when Listen is not loopback.
	TokenEnv string `json:"token_env,omitempty"`
}

// MetricsToken is the bearer token server.metrics.token_env names, if any.
func (m ServerMetricsConfig) MetricsToken() string {
	if m.TokenEnv == "" {
		return ""
	}
	return os.Getenv(m.TokenEnv)
}

// validate checks server.metrics for what can be told without listening.
func (m ServerMetricsConfig) validate() error {
	if m.Listen == "" {
		if m.TokenEnv != "" {
			return fmt.Errorf("server.metrics.token_env is for server.metrics.listen, which is unset")
		}
		return nil
	}
	host, port, err := net.SplitHostPort(m.Listen)
	if err != nil || port == "" {
		return fmt.Errorf("server.metrics.listen is %q; use host:port, such as 127.0.0.1:9464", m.Listen)
	}
	if m.TokenEnv == "" && !loopbackHost(host) {
		return fmt.Errorf("server.metrics.listen %q is reachable from other machines; name a bearer token with server.metrics.token_env, or listen on 127.0.0.1", m.Listen)
	}
	return nil
}

// loopbackHost reports whether host names only this machine.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}
