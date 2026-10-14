package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables an edition read before these settings were keys;
// each applies only where the configuration leaves its key unset.
const (
	EnvOIDCBoundClaims    = "ABHED_OIDC_BOUND_CLAIMS"
	EnvHARelayCertFile    = "ABHED_HA_RELAY_CERT_FILE"
	EnvHARelayCertKeyFile = "ABHED_HA_RELAY_CERT_KEY_FILE"
	EnvHARelayPeer        = "ABHED_HA_RELAY_PEER"
	EnvHARelayKeyVault    = "ABHED_HA_RELAY_KEY_VAULT"
	EnvHARelayKeyCommand  = "ABHED_HA_RELAY_KEY_COMMAND"
)

// ValidateBoundClaims checks claim rules named by source: each claim has a
// name and at least one value, and no value admits every value.
func ValidateBoundClaims(source string, bound map[string][]string) error {
	for name, values := range bound {
		if strings.TrimSpace(name) == "" || len(values) == 0 {
			return fmt.Errorf("%s: claim %q needs at least one value", source, name)
		}
		for _, v := range values {
			if strings.TrimSuffix(v, "*") == "" {
				return fmt.Errorf("%s: %s %q admits every value; name one", source, name, v)
			}
		}
	}
	return nil
}

// OIDCBoundClaims are the claim rules bearer tokens must meet:
// auth.bound_claims, else ABHED_OIDC_BOUND_CLAIMS. source names where they
// came from, for messages; "" with none.
func (c Config) OIDCBoundClaims() (bound map[string][]string, source string, err error) {
	if len(c.Auth.BoundClaims) > 0 {
		return c.Auth.BoundClaims, "auth.bound_claims", nil
	}
	raw := strings.TrimSpace(os.Getenv(EnvOIDCBoundClaims))
	if raw == "" {
		return nil, "", nil
	}
	if err := CheckManagedJSON([]byte(raw)); err != nil {
		return nil, EnvOIDCBoundClaims, fmt.Errorf("%s: %w", EnvOIDCBoundClaims, err)
	}
	if err := json.Unmarshal([]byte(raw), &bound); err != nil {
		return nil, EnvOIDCBoundClaims, fmt.Errorf("%s: want a JSON object of claim names to lists of values: %w", EnvOIDCBoundClaims, err)
	}
	return bound, EnvOIDCBoundClaims, ValidateBoundClaims(EnvOIDCBoundClaims, bound)
}

// RelayKeyCommandArgv reads ABHED_HA_RELAY_KEY_COMMAND: a JSON array of a
// program and its arguments, or one path.
func RelayKeyCommandArgv(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if !strings.HasPrefix(v, "[") {
		return []string{v}, nil
	}
	var argv []string
	if err := json.Unmarshal([]byte(v), &argv); err != nil || len(argv) == 0 {
		return nil, fmt.Errorf("%s: want a JSON array of strings or one absolute path", EnvHARelayKeyCommand)
	}
	return argv, nil
}

// withRelayEnv fills the relay settings the configuration leaves unset from
// the environment. A key command that does not parse is kept as written, so
// the check of its program refuses it.
func (h HAConfig) withRelayEnv() HAConfig {
	fill := func(dst *string, env string) {
		if *dst == "" {
			*dst = strings.TrimSpace(os.Getenv(env))
		}
	}
	fill(&h.RelayCertFile, EnvHARelayCertFile)
	fill(&h.RelayCertKeyFile, EnvHARelayCertKeyFile)
	fill(&h.RelayPeer, EnvHARelayPeer)
	fill(&h.RelayKeyVault, EnvHARelayKeyVault)
	if len(h.RelayKeyCommand) == 0 {
		if raw := os.Getenv(EnvHARelayKeyCommand); strings.TrimSpace(raw) != "" {
			argv, err := RelayKeyCommandArgv(raw)
			if err != nil {
				argv = []string{raw}
			}
			h.RelayKeyCommand = argv
		}
	}
	return h
}

// validateRelay checks the relay settings the configuration makes.
func (h HAConfig) validateRelay() error {
	switch {
	case (h.RelayCertFile == "") != (h.RelayCertKeyFile == ""):
		return fmt.Errorf("control_plane.ha: set both relay_cert_file and relay_cert_key_file, or neither")
	case h.RelayPeer != "" && h.RelayCertFile == "":
		return fmt.Errorf("control_plane.ha.relay_peer needs relay_cert_file and relay_cert_key_file: a replica that requires the others' certificates presents its own")
	}
	if v := h.RelayKeyVault; v != "" {
		if entry, field, ok := strings.Cut(strings.TrimSpace(v), "#"); !ok || entry == "" || field == "" {
			return fmt.Errorf("control_plane.ha.relay_key_vault is %q; want entry#field, such as ha-relay#key", v)
		}
	}
	if len(h.RelayKeyCommand) > 0 && !filepath.IsAbs(h.RelayKeyCommand[0]) {
		return fmt.Errorf("control_plane.ha.relay_key_command must start with an absolute path, not %q", h.RelayKeyCommand[0])
	}
	return nil
}
