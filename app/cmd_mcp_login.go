package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/mcpauth"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// mcpOAuthHTTP is the client a sign-in reaches the server and its
// authorization server with: under the configuration's egress allowlist and
// the upstream proxy, following no redirect; a variable so tests can bound it.
var mcpOAuthHTTP = func(guard *egress.Guard) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &egress.Transport{Kind: egress.KindMCP, Guard: guard},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// mcpURLServer is the URL server name in effect in this workspace, after
// the MCP lists: one they refuse is not signed in to.
func mcpURLServer(cfg config.Config, name string) (config.MCPServerConfig, error) {
	for _, s := range cfg.MCP.Servers {
		if s.Name != name {
			continue
		}
		if s.URL == "" {
			return s, fmt.Errorf("MCP server %s runs a program; only a URL server signs in", name)
		}
		if err := cfg.MCPAdmitted(config.MCPServerOf(s)); err != nil {
			return s, err
		}
		return s, nil
	}
	return config.MCPServerConfig{}, fmt.Errorf("no MCP server named %s is configured here (or the MCP server lists refuse it); abhed mcp list shows them", config.Printable(name))
}

// mcpLogin is `abhed mcp login NAME`: sign in to a URL server with OAuth in
// the person's browser, keeping the tokens in the secret store.
func mcpLogin(workspace string, trust config.TrustChoice, name string, cio mcpIO) error {
	if cio.inSandbox {
		return errors.New("abhed mcp login is refused inside an agent's command; run it yourself at a terminal")
	}
	if !cio.terminal {
		return errors.New("abhed mcp login signs in through your browser and needs a person at a terminal")
	}
	if cio.byEditor {
		// The editor asked already; no yes on the descriptor is a failure.
		line, _ := bufio.NewReader(cio.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("no yes was read from -confirm-fd; nothing was signed in")
		}
	}
	cfg, err := loadNetworked(workspace, config.LoadOptions{Trust: trust, Quiet: true})
	if err != nil {
		return err
	}
	s, err := mcpURLServer(cfg, name)
	if err != nil {
		return err
	}
	o := mcpauth.LoginOptions{Server: s.Name, URL: s.URL, Out: cio.out}
	if s.OAuth != nil {
		o.ClientID, o.Scopes = s.OAuth.ClientID, s.OAuth.Scopes
		if s.OAuth.ClientSecretEnv != "" {
			if o.ClientSecret = os.Getenv(s.OAuth.ClientSecretEnv); o.ClientSecret == "" {
				return fmt.Errorf("oauth.client_secret_env names %s, which is not set", config.Printable(s.OAuth.ClientSecretEnv))
			}
		}
	}
	guard := toolset.OwnEgress(cfg)
	if guard != nil {
		defer guard.Close()
		defer egress.Install(guard)()
	}
	o.HTTP = mcpOAuthHTTP(guard)
	// With -no-browser, no browser here, or an editor that asked (it opens the address itself), it is only printed.
	if !cio.byEditor && !cio.noBrowser && canOpenBrowser() {
		o.Open = openBrowser
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := mcpauth.Login(ctx, openVault(), o); err != nil {
		return fmt.Errorf("sign in to MCP server %s: %w", s.Name, err)
	}
	by := "login"
	if cio.byEditor {
		by = "login -confirm-fd"
	}
	logMCPAuth(by, s, cio)
	return nil
}

// mcpLogout is `abhed mcp logout NAME`: revoke and forget a server's login.
func mcpLogout(workspace string, trust config.TrustChoice, name string, cio mcpIO) error {
	if cio.inSandbox {
		return errors.New("abhed mcp logout is refused inside an agent's command; run it yourself at a terminal")
	}
	if !mcpServerName.MatchString(name) {
		return fmt.Errorf("%q is not an MCP server name", config.Printable(name))
	}
	// Forgetting needs no configuration; revoking reaches the network as a login does.
	var guard *egress.Guard
	s := config.MCPServerConfig{Name: name}
	if cfg, err := loadNetworked(workspace, config.LoadOptions{Trust: trust, Quiet: true}); err == nil {
		if guard = toolset.OwnEgress(cfg); guard != nil {
			defer guard.Close()
			defer egress.Install(guard)()
		}
		for _, c := range cfg.MCP.Servers {
			if c.Name == name {
				s = c
			}
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	found, err := mcpauth.Logout(ctx, openVault(), name, mcpOAuthHTTP(guard))
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(cio.out, "MCP server %s has no login.\n", name)
		return nil
	}
	fmt.Fprintf(cio.out, "Signed out of MCP server %s.\n", name)
	logMCPAuth("logout", s, cio)
	return nil
}

// logMCPAuth appends a login or logout to the person's config change log,
// with the server's name and transport, never a token; a log that cannot be
// written is said, and the sign-in stands.
func logMCPAuth(by string, s config.MCPServerConfig, cio mcpIO) {
	home, err := os.UserHomeDir()
	if err == nil {
		err = logConfigChange(by, "mcp.oauth", s, filepath.Join(home, ".abhed", "config.json"))
	}
	if err != nil {
		fmt.Fprintf(cio.err, "abhed: warning: the %s was not written to the config change log: %v\n", by, err)
	}
}

// accountsSignIn reports a server whose accounts sign in, each with their
// own secrets, as abhed serve keeps them.
func accountsSignIn(cfg config.Config) bool {
	return cfg.Auth.Mode != "" && cfg.Auth.Mode != "none"
}

// mcpLoginsHeld are the configured URL servers that have an OAuth login.
func mcpLoginsHeld(cfg config.Config, store *secrets.Store) []string {
	var out []string
	for _, s := range cfg.MCP.Servers {
		if s.URL == "" || !s.Enabled {
			continue
		}
		if in, _, _, err := mcpauth.Status(store, s.Name, s.URL); in || err != nil {
			out = append(out, s.Name)
		}
	}
	return out
}

// serveMCPLogins is why abhed serve's shared tool set uses no MCP login, or
// nil: with accounts signing in, an operator's login would let each act as
// the operator. Each server that has one is warned of on errOut.
func serveMCPLogins(cfg config.Config, store *secrets.Store, errOut io.Writer) error {
	if !accountsSignIn(cfg) {
		return nil
	}
	for _, name := range mcpLoginsHeld(cfg, store) {
		fmt.Fprintf(errOut, "abhed: warning: MCP server %s has an OAuth login, which this server does not use: %v\n", name, toolset.ErrMCPLoginsPerAccount)
	}
	return toolset.ErrMCPLoginsPerAccount
}
