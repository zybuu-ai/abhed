package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/managed"
)

const testProxy = "proxy.corp.test:3128"

// networkHome is a home whose own configuration names network, with no
// managed file and no proxy in force; it returns a workspace.
func networkHome(t *testing.T, network string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	t.Setenv(config.TrustEnv, "")
	old := managed.ConfigFile
	managed.ConfigFile = filepath.Join(t.TempDir(), "absent.json")
	t.Cleanup(func() { managed.ConfigFile = old })
	egress.ResetUpstreamForTest()
	t.Cleanup(egress.ResetUpstreamForTest)
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".abhed", "config.json"), []byte(`{"network":`+network+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return t.TempDir()
}

func proxyInForce() string {
	if up := egress.CurrentUpstream(); up != nil {
		return up.Authority()
	}
	return ""
}

// Every command that makes a connection of Abhed's own puts the configured
// upstream proxy in force before it connects.
func TestNetworkedCommandsPutTheProxyInForce(t *testing.T) {
	for name, run := range map[string]func(ws string) error{
		"record list": func(ws string) error {
			recordCmd(ws, []string{"list"}, config.TrustRefused, strings.NewReader(""), io.Discard, io.Discard)
			return nil
		},
		"record ship status": func(ws string) error {
			recordCmd(ws, []string{"ship", "status"}, config.TrustRefused, strings.NewReader(""), io.Discard, io.Discard)
			return nil
		},
		"hawkeye store": func(ws string) error {
			_, done, err := hawkeyeStore(ws, config.TrustRefused)
			if err == nil {
				done()
			}
			return err
		},
		"policy refresh": func(string) error {
			policyCmd([]string{"refresh"}, io.Discard, io.Discard)
			return nil
		},
		"mcp list": func(ws string) error { return mcpList(ws, config.TrustRefused, io.Discard) },
		"profile list": func(ws string) error {
			profileCmd(ws, []string{"list"}, io.Discard, io.Discard)
			return nil
		},
		"session start": func(ws string) error {
			return orgPolicyAfterNetwork(context.Background(), ws, config.LoadOptions{Quiet: true}, io.Discard)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws := networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`"}}`)
			fetchOrgAtStart = func(context.Context, io.Writer) {}
			t.Cleanup(func() { fetchOrgAtStart = orgPolicyAtStart })
			if err := run(ws); err != nil {
				t.Fatal(err)
			}
			if got := proxyInForce(); got != testProxy {
				t.Fatalf("the proxy in force is %q", got)
			}
		})
	}
}

// At a session's start the proxy goes in force before the org policy is
// fetched; a network configuration that cannot go in force fetches nothing.
func TestOrgPolicyFetchFollowsTheNetwork(t *testing.T) {
	ws := networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`"}}`)
	var seen []string
	fetchOrgAtStart = func(context.Context, io.Writer) { seen = append(seen, proxyInForce()) }
	t.Cleanup(func() { fetchOrgAtStart = orgPolicyAtStart })
	if err := orgPolicyAfterNetwork(context.Background(), ws, config.LoadOptions{Quiet: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != testProxy {
		t.Fatalf("the fetch ran with %v in force", seen)
	}

	seen = nil
	ws = networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`","ca_file":"/nonexistent/corp.pem"}}`)
	if err := orgPolicyAfterNetwork(context.Background(), ws, config.LoadOptions{Quiet: true}, io.Discard); err == nil || len(seen) != 0 {
		t.Fatalf("a broken network configuration: %v, fetched %v", err, seen)
	}
}

// An editor connection whose network configuration cannot go in force
// starts neither the org policy fetch nor its periodic refresh, and its
// sessions fetch nothing either; one that can starts both.
func TestACPConnectOrgPolicyFollowsTheNetwork(t *testing.T) {
	var fetched, refreshing int
	fetchOrgAtStart = func(context.Context, io.Writer) { fetched++ }
	startOrgRefresh = func(context.Context, time.Duration, io.Writer) { refreshing++ }
	t.Cleanup(func() { fetchOrgAtStart, startOrgRefresh = orgPolicyAtStart, keepOrgPolicyFresh })

	networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`","ca_file":"/nonexistent/corp.pem"}}`)
	c := &acpConn{ctx: context.Background(), out: io.Discard, sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	c.orgPolicyAtConnect(io.Discard)
	if !c.noNetwork || fetched != 0 || refreshing != 0 || proxyInForce() != "" {
		t.Fatalf("noNetwork %v, fetched %d, refreshing %d, proxy %q", c.noNetwork, fetched, refreshing, proxyInForce())
	}
	if _, e := c.openSession(openOptions{cwd: t.TempDir()}); e == nil || fetched != 0 {
		t.Fatalf("a session opened (%v) or fetched (%d) around the proxy", e, fetched)
	}

	networkHome(t, `{"upstream_proxy":{"url":"http://`+testProxy+`"}}`)
	c = &acpConn{ctx: context.Background(), out: io.Discard, sessions: map[string]*acpSession{}, pending: map[int64]chan rpcMessage{}}
	c.orgPolicyAtConnect(io.Discard)
	if c.noNetwork || fetched != 1 || refreshing != 1 || proxyInForce() != testProxy {
		t.Fatalf("noNetwork %v, fetched %d, refreshing %d, proxy %q", c.noNetwork, fetched, refreshing, proxyInForce())
	}
}
