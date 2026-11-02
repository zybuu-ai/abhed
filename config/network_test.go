package config

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// testBundle writes a PEM bundle of one certificate and returns its path.
func testBundle(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(nil)
	srv.Close()
	path := filepath.Join(t.TempDir(), "corp.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadSettings(t *testing.T, body string) (Config, error) {
	t.Helper()
	t.Cleanup(func() { egress.SetUpstream(nil) })
	return LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(body), SettingsName: "s.json"})
}

// network.upstream_proxy loads into the process's upstream proxy, with its
// CA bundle and no_proxy list; a malformed one stops the load, and one with
// a credential in the URL is refused without echoing it.
func TestUpstreamProxyConfig(t *testing.T) {
	bundle := testBundle(t)
	cfg, err := loadSettings(t, `{"network":{"upstream_proxy":{"url":"http://proxy.corp.test:3128","ca_file":"`+bundle+
		`","credentials_secret":"CORP_PROXY","no_proxy":[".corp.test","10.0.0.0/8"]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	up := egress.CurrentUpstream()
	if up == nil || up.URL.String() != "http://proxy.corp.test:3128" || up.Credentials == nil || up.Roots == nil ||
		len(up.Bundle) != 1 || !slices.Equal(up.NoProxy, []string{".corp.test", "10.0.0.0/8"}) {
		t.Fatalf("upstream %+v from %+v", up, cfg.Network.UpstreamProxy)
	}
	if _, _, _, err := up.Credentials(); err == nil || !strings.Contains(err.Error(), "CORP_PROXY") {
		t.Fatalf("a missing secret: %v", err)
	}
	// A configuration without one leaves none in force.
	if _, err := loadSettings(t, `{}`); err != nil || egress.CurrentUpstream() != nil {
		t.Fatalf("no proxy: %v %v", err, egress.CurrentUpstream())
	}

	for body, want := range map[string]string{
		`{"network":{"upstream_proxy":{"url":"http://bob:hunter22@proxy:3128"}}}`:                    "credentials_secret",
		`{"network":{"upstream_proxy":{"url":"socks5://proxy:1080"}}}`:                               "SOCKS",
		`{"network":{"upstream_proxy":{"url":"http://proxy:3128/proxy.pac"}}}`:                       "PAC",
		`{"network":{"upstream_proxy":{"url":""}}}`:                                                  "empty",
		`{"network":{"upstream_proxy":{"url":"http://proxy:3128","ca_file":"/nonexistent"}}}`:        "ca_file",
		`{"network":{"upstream_proxy":{"url":"http://proxy:3128","no_proxy":["*"]}}}`:                "may not be *",
		`{"network":{"upstream_proxy":{"url":"http://proxy:3128","no_proxy":["host:443"]}}}`:         "port",
		`{"network":{"upstream_proxy":{"url":"http://proxy:3128","no_proxy":["10.0.0.0/33"]}}}`:      "CIDR",
		`{"network":{"upstream_proxy":{"url":"http://proxy:3128","credentials_secret":"bad name"}}}`: "secret name",
	} {
		_, err := loadSettings(t, body)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
		if err != nil && strings.Contains(err.Error(), "hunter22") {
			t.Errorf("the error echoed the password: %v", err)
		}
	}
}

// A workspace never sets the proxy, trusted or not: it would route the
// model's traffic and trust a CA of its choosing.
func TestUpstreamProxyNotFromAWorkspace(t *testing.T) {
	t.Cleanup(func() { egress.SetUpstream(nil) })
	for _, trust := range []bool{false, true} {
		_, ws := trustHome(t, `{}`, `{"network":{"upstream_proxy":{"url":"http://attacker.test:8080"}}}`)
		o := LoadOptions{Quiet: true}
		if trust {
			o.Trust = TrustGranted
		}
		cfg, err := LoadWith(ws, o)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Network.UpstreamProxy != nil || egress.CurrentUpstream() != nil {
			t.Fatalf("trusted %v: a workspace set the proxy: %+v", trust, cfg.Network.UpstreamProxy)
		}
	}
}

// The managed file can force the proxy: it wins over the person's, and the
// person's no_proxy list cannot widen what reaches the network directly.
func TestUpstreamProxyManaged(t *testing.T) {
	t.Cleanup(func() { egress.SetUpstream(nil) })
	withManaged(t, `{"network":{"upstream_proxy":{"url":"http://proxy.corp.test:3128"}}}`)
	body := []byte(`{"network":{"upstream_proxy":{"url":"http://mine.test:1","no_proxy":["*.example.com"],"credentials_secret":"MINE"}}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "s.json"})
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Network.UpstreamProxy
	if p == nil || p.URL != "http://proxy.corp.test:3128" || len(p.NoProxy) != 0 || p.CredentialsSecret != "MINE" {
		t.Fatalf("managed proxy: %+v", p)
	}
	if up := egress.CurrentUpstream(); up == nil || up.Authority() != "proxy.corp.test:3128" || len(up.NoProxy) != 0 {
		t.Fatalf("in force: %+v", up)
	}
	withManaged(t, `{"network":{"upstream_proxy":{"url":"http://proxy.corp.test:3128","no_proxy":[".corp.test"]}}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "s.json"})
	if err != nil || !slices.Equal(cfg.Network.UpstreamProxy.NoProxy, []string{".corp.test"}) {
		t.Fatalf("managed no_proxy: %+v %v", cfg.Network.UpstreamProxy, err)
	}
	// A program that reads the managed file alone gets it too.
	egress.SetUpstream(nil)
	if _, err := LoadManaged(); err != nil || egress.CurrentUpstream() == nil {
		t.Fatalf("LoadManaged: %v %v", err, egress.CurrentUpstream())
	}
}

func TestUpstreamBundleIsChecked(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a certificate"), 0o600)
	if _, err := loadSettings(t, `{"network":{"upstream_proxy":{"url":"https://proxy:443","ca_file":"`+bad+`"}}}`); err == nil ||
		!strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("bad bundle: %v", err)
	}
}
