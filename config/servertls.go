package config

import (
	"os"
	"path/filepath"
)

// serverPath resolves a server file setting against the workspace, as
// auth.users_file is, so serve finds one file whichever directory it starts in.
func serverPath(workspace, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workspace, p)
}

// ResolvedTLS is server.tls with every path made absolute for workspace.
func (c Config) ResolvedTLS(workspace string) ServerTLSConfig {
	t := c.Server.TLS
	t.CertFile = serverPath(workspace, t.CertFile)
	t.KeyFile = serverPath(workspace, t.KeyFile)
	t.ClientCAFile = serverPath(workspace, t.ClientCAFile)
	extra := make([]CertificatePair, len(t.AdditionalCertificates))
	for i, p := range t.AdditionalCertificates {
		extra[i] = CertificatePair{CertFile: serverPath(workspace, p.CertFile), KeyFile: serverPath(workspace, p.KeyFile)}
	}
	t.AdditionalCertificates = extra
	return t
}

// CLIKeyFile is where command-line tokens' signing key is kept:
// auth.cli.key_file, else ~/.abhed/cli-token.key.
func (c Config) CLIKeyFile(workspace string) (string, error) {
	if p := c.Auth.CLI.KeyFile; p != "" {
		return serverPath(workspace, p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".abhed", "cli-token.key"), nil
}

// ServerSecretFiles are the private files serve reads that commands must not
// reach: the TLS keys and, with command-line sign-in, its token key and the
// list of tokens signed out early.
func (c Config) ServerSecretFiles(workspace string) []string {
	var out []string
	if c.Server.TLS.Enabled() {
		t := c.ResolvedTLS(workspace)
		if t.KeyFile != "" {
			out = append(out, t.KeyFile)
		}
		for _, p := range t.AdditionalCertificates {
			if p.KeyFile != "" {
				out = append(out, p.KeyFile)
			}
		}
	}
	if c.Auth.CLI.Enabled {
		if p, err := c.CLIKeyFile(workspace); err == nil {
			out = append(out, p, p+".revoked")
		}
	}
	return out
}
