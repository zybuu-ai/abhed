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
	t.ClientRevokedFile = serverPath(workspace, t.ClientRevokedFile)
	extra := make([]CertificatePair, len(t.AdditionalCertificates))
	for i, p := range t.AdditionalCertificates {
		extra[i] = CertificatePair{CertFile: serverPath(workspace, p.CertFile), KeyFile: serverPath(workspace, p.KeyFile)}
	}
	t.AdditionalCertificates = extra
	return t
}

// ResolvedHA is control_plane.ha with the relay settings it leaves unset
// taken from their ABHED_HA_RELAY_* variables, and its files made absolute
// for workspace: the files serve protects as state, and the ones an
// edition's replicas read.
func (c Config) ResolvedHA(workspace string) HAConfig {
	h := c.ControlPlane.HA.withRelayEnv()
	h.RelayKeyFile = serverPath(workspace, h.RelayKeyFile)
	h.RelayCAFile = serverPath(workspace, h.RelayCAFile)
	h.RelayCertFile = serverPath(workspace, h.RelayCertFile)
	h.RelayCertKeyFile = serverPath(workspace, h.RelayCertKeyFile)
	return h
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

// CLIEditionStateSuffix names the file beside a token key where an edition
// keeps state of its own, such as tokens it signed out early.
const CLIEditionStateSuffix = ".edition"

// ServerSecretFiles are the private files serve reads that commands must not
// reach: the TLS keys and the client CA; with command-line sign-in, its token
// key and the list of tokens signed out early; with workload identity, its
// token key and the trust bundles; with replicas, the relay's key and CA.
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
		// Whoever could rewrite the client CA, which serve reloads, could
		// mint a certificate for any account.
		if t.ClientCAFile != "" {
			out = append(out, t.ClientCAFile)
		}
		// Whoever could rewrite the revocation file could restore a revoked certificate.
		if t.ClientRevokedFile != "" {
			out = append(out, t.ClientRevokedFile)
		}
	}
	if ha := c.ResolvedHA(workspace); ha.Enabled {
		for _, p := range []string{ha.RelayKeyFile, ha.RelayCAFile, ha.RelayCertKeyFile} {
			if p != "" {
				out = append(out, p)
			}
		}
	}
	if c.Auth.CLI.Enabled {
		if p, err := c.CLIKeyFile(workspace); err == nil {
			out = append(out, p, p+".revoked", p+CLIEditionStateSuffix)
		}
	}
	// Whoever could replace a trust bundle could mint workload identities.
	if w := c.ResolvedWorkload(workspace); w.Enabled {
		if p, err := c.WorkloadKeyFile(workspace); err == nil {
			out = append(out, p, p+CLIEditionStateSuffix)
		}
		for _, p := range []string{w.JWTBundleFile, w.X509BundleFile} {
			if p != "" {
				out = append(out, p)
			}
		}
	}
	// Whoever reads the relay's host key can pose as every sandbox's SSH server.
	if c.ControlPlane.Enabled && c.ControlPlane.SSHHostKey != "" {
		if p, err := c.SSHHostKeyFile(); err == nil {
			out = append(out, p)
		}
	}
	return out
}
