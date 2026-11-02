package config

import (
	"encoding/pem"
	"errors"
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
	t.Cleanup(egress.ResetUpstreamForTest)
	return LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: []byte(body), SettingsName: "s.json"})
}

// network.upstream_proxy builds the upstream proxy, with its CA bundle,
// credentials and no_proxy list; loading puts nothing in force, and
// ApplyNetwork does, once per process.
func TestUpstreamProxyConfig(t *testing.T) {
	bundle := testBundle(t)
	cfg, err := loadSettings(t, `{"network":{"upstream_proxy":{"url":"http://proxy.corp.test:3128","ca_file":"`+bundle+
		`","credentials_secret":"CORP_PROXY","no_proxy":[".corp.test","10.0.0.0/8"],"resolve_via_proxy":true}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if egress.CurrentUpstream() != nil {
		t.Fatal("loading the configuration changed the process's upstream proxy")
	}
	if err := cfg.ApplyNetwork(); err != nil {
		t.Fatal(err)
	}
	up := egress.CurrentUpstream()
	if up == nil || up.URL.String() != "http://proxy.corp.test:3128" || up.Credentials == nil || up.Roots == nil ||
		len(up.Bundle) != 1 || !slices.Equal(up.NoProxy, []string{".corp.test", "10.0.0.0/8"}) || !up.ResolveViaProxy {
		t.Fatalf("upstream %+v from %+v", up, cfg.Network.UpstreamProxy)
	}
	if _, _, _, err := up.Credentials(); err == nil || !strings.Contains(err.Error(), "CORP_PROXY") {
		t.Fatalf("a missing secret: %v", err)
	}
	// The same again is accepted; another, or none, in the same process is refused.
	if err := cfg.ApplyNetwork(); err != nil {
		t.Fatalf("the same proxy again: %v", err)
	}
	other, err := loadSettings(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	egress.SetUpstream(up) // loadSettings' cleanup runs at the end, not here
	if err := other.ApplyNetwork(); !errors.Is(err, egress.ErrUpstreamConflict) || egress.CurrentUpstream() != up {
		t.Fatalf("a second, different proxy: %v", err)
	}
	// Loading the managed configuration alone leaves it as it is.
	if _, err := LoadManaged(); err != nil || egress.CurrentUpstream() != up {
		t.Fatalf("LoadManaged changed the upstream: %v", err)
	}

	for body, want := range map[string]string{
		`{"network":{"upstream_proxy":{"url":"http://bob:hunter22@proxy:3128"}}}`:                    "credentials_secret",
		`{"network":{"upstream_proxy":{"url":"socks5://proxy:1080"}}}`:                               "SOCKS",
		`{"network":{"upstream_proxy":{"url":"http://proxy:3128/proxy.pac"}}}`:                       "PAC",
		`{"network":{"upstream_proxy":{"url":""}}}`:                                                  "empty",
		`{"network":{"upstream_proxy":{"url":"http://prøxy.corp:3128"}}}`:                            "ASCII",
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
	for _, trust := range []bool{false, true} {
		_, ws := trustHome(t, `{}`, `{"network":{"upstream_proxy":{"url":"http://attacker.test:8080","resolve_via_proxy":true}}}`)
		o := LoadOptions{Quiet: true}
		if trust {
			o.Trust = TrustGranted
		}
		cfg, err := LoadWith(ws, o)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Network.UpstreamProxy != nil {
			t.Fatalf("trusted %v: a workspace set the proxy: %+v", trust, cfg.Network.UpstreamProxy)
		}
	}
}

// The managed file can force the proxy: it wins over the person's, and the
// person's no_proxy list and resolve_via_proxy cannot widen it; the person's
// credentials name stays, as their own account at the proxy.
func TestUpstreamProxyManaged(t *testing.T) {
	withManaged(t, `{"network":{"upstream_proxy":{"url":"http://proxy.corp.test:3128"}}}`)
	body := []byte(`{"network":{"upstream_proxy":{"url":"http://mine.test:1","no_proxy":["*.example.com"],"credentials_secret":"MINE","resolve_via_proxy":true}}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "s.json"})
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Network.UpstreamProxy
	if p == nil || p.URL != "http://proxy.corp.test:3128" || len(p.NoProxy) != 0 || p.CredentialsSecret != "MINE" || p.ResolveViaProxy {
		t.Fatalf("managed proxy: %+v", p)
	}
	withManaged(t, `{"network":{"upstream_proxy":{"url":"http://proxy.corp.test:3128","no_proxy":[".corp.test"],"resolve_via_proxy":true}}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "s.json"})
	if err != nil || !slices.Equal(cfg.Network.UpstreamProxy.NoProxy, []string{".corp.test"}) || !cfg.Network.UpstreamProxy.ResolveViaProxy {
		t.Fatalf("managed no_proxy and resolve_via_proxy: %+v %v", cfg.Network.UpstreamProxy, err)
	}
	// The managed file alone gives it too, credentials and all it names.
	m, err := LoadManaged()
	if err != nil || m.Network.UpstreamProxy == nil || m.Network.UpstreamProxy.URL != "http://proxy.corp.test:3128" {
		t.Fatalf("LoadManaged: %+v %v", m.Network.UpstreamProxy, err)
	}
}

// A managed upstream_proxy written as {} is refused (it names no URL), and
// as null leaves the person's proxy with its no_proxy as it is.
func TestUpstreamProxyManagedEmptyAndNull(t *testing.T) {
	body := []byte(`{"network":{"upstream_proxy":{"url":"http://mine.test:1","no_proxy":[".mine.test"]}}}`)
	withManaged(t, `{"network":{"upstream_proxy":{}}}`)
	if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "s.json"}); err != nil {
		// The person's URL stays under an empty managed object, so this loads.
		t.Fatalf("managed {}: %v", err)
	}
	withManaged(t, `{"network":{"upstream_proxy":{}}}`)
	if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true}); err == nil {
		t.Fatal("a managed {} with no URL anywhere loaded")
	}
	withManaged(t, `{"network":{"upstream_proxy":null}}`)
	cfg, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true, Settings: body, SettingsName: "s.json"})
	if err != nil {
		t.Fatalf("managed null: %v", err)
	}
	if cfg.Network.UpstreamProxy != nil {
		t.Fatalf("a managed null left a proxy: %+v", cfg.Network.UpstreamProxy)
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
