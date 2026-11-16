package config

import (
	"fmt"
	"regexp"
)

var (
	// oauthClientID is a client_id as a configuration may name one: printable
	// ASCII without space, as RFC 6749's VSCHAR.
	oauthClientID = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)
	// oauthScope is one scope (RFC 6749 §3.3).
	oauthScope = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]{1,128}$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// validateMCPOAuth refuses an oauth block on a server that runs a program,
// or one whose client, secret variable or scopes cannot be what they say.
func (c Config) validateMCPOAuth() error {
	for _, s := range c.MCP.Servers {
		o := s.OAuth
		if o == nil {
			continue
		}
		name := Printable(s.Name)
		switch {
		case s.URL == "":
			return fmt.Errorf("mcp server %s sets oauth, but runs a program: only a URL server signs in", name)
		case o.ClientID != "" && !oauthClientID.MatchString(o.ClientID):
			return fmt.Errorf("mcp server %s: oauth.client_id holds characters a client id cannot", name)
		case o.ClientSecretEnv != "" && !envName.MatchString(o.ClientSecretEnv):
			return fmt.Errorf("mcp server %s: oauth.client_secret_env is not an environment variable's name", name)
		case o.ClientSecretEnv != "" && o.ClientID == "":
			return fmt.Errorf("mcp server %s: oauth.client_secret_env needs oauth.client_id", name)
		}
		for _, sc := range o.Scopes {
			if !oauthScope.MatchString(sc) {
				return fmt.Errorf("mcp server %s: oauth.scopes holds %q, which is not a scope", name, Printable(sc))
			}
		}
	}
	return nil
}
