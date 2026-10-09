package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/servetls"
)

// buildTLS reads server.tls, or returns nil when it sets nothing. Any setting
// made and not usable is an error, so serve never falls back to plain HTTP.
func buildTLS(cfg config.Config, workspace string, log *slog.Logger) (*servetls.Listener, error) {
	if !cfg.Server.TLS.Enabled() {
		return nil, nil
	}
	t := cfg.ResolvedTLS(workspace)
	spec := servetls.Spec{
		Certs:        []servetls.Pair{{CertFile: t.CertFile, KeyFile: t.KeyFile}},
		ClientCAFile: t.ClientCAFile,
		Interval:     time.Duration(t.ReloadSeconds) * time.Second,
		Logger:       log,
	}
	for _, p := range t.AdditionalCertificates {
		spec.Certs = append(spec.Certs, servetls.Pair{CertFile: p.CertFile, KeyFile: p.KeyFile})
	}
	if t.MinVersion == "1.3" {
		spec.MinVersion = tls.VersionTLS13
	}
	if t.ClientCAFile != "" {
		spec.ClientAuth = servetls.RequireClientCert
		if t.ClientAuth == "optional" {
			spec.ClientAuth = servetls.OptionalClientCert
		}
	}
	l, err := servetls.New(spec)
	if err != nil {
		return nil, fmt.Errorf("server.tls is set and cannot be used, so serve will not start: %w", err)
	}
	return l, nil
}

// tlsLabel describes the listener for the banner.
func tlsLabel(cfg config.Config, l *servetls.Listener) string {
	if l == nil {
		return "off (plain HTTP; terminate TLS in front of this server)"
	}
	s := "on, TLS " + orDefault(cfg.Server.TLS.MinVersion, "1.2") + "+"
	if n := len(l.Leaves()); n > 1 {
		s += fmt.Sprintf(", %d certificates by SNI", n)
	}
	switch {
	case cfg.Server.TLS.ClientCAFile == "":
	case cfg.Server.TLS.ClientAuth == "optional":
		s += ", client certificates optional"
	default:
		s += ", client certificates required"
	}
	if cfg.Server.TLS.ClientCertAccounts {
		s += " (sign local accounts in)"
	}
	return s
}

// addClientCertAuth puts certificate sign-in after local accounts' cookie,
// so a browser session still decides who a browser is.
func addClientCertAuth(cfg config.Config, mw *auth.Middleware) error {
	if !cfg.Server.TLS.ClientCertAccounts {
		return nil
	}
	local := localAuthOf(mw)
	if local == nil {
		return errors.New("server.tls.client_cert_accounts needs local accounts (auth.mode local)")
	}
	mw.Providers = append(mw.Providers, auth.NewClientCertAuth(local))
	return nil
}

func localAuthOf(mw *auth.Middleware) *auth.LocalAuth {
	if mw == nil {
		return nil
	}
	for _, p := range mw.Providers {
		if l, ok := p.(*auth.LocalAuth); ok {
			return l
		}
	}
	return nil
}

// buildCLILogin serves `abhed login` for local accounts when auth.cli asks:
// its tokens join the middleware's verifiers and its start and token
// endpoints become public.
func buildCLILogin(cfg config.Config, workspace string, mw *auth.Middleware) (*auth.CLILogin, error) {
	if !cfg.Auth.CLI.Enabled {
		return nil, nil
	}
	local := localAuthOf(mw)
	if local == nil {
		return nil, errors.New("auth.cli.enabled needs local accounts (auth.mode local)")
	}
	key, err := cfg.CLIKeyFile(workspace)
	if err != nil {
		return nil, err
	}
	tokens, err := auth.OpenCLITokens(local, key, time.Duration(cfg.Auth.CLI.TokenHours)*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("auth.cli: %w", err)
	}
	login := auth.NewCLILogin(tokens)
	mw.Verifier = auth.ChainVerifier(mw.Verifier, tokens)
	mw.PublicPaths = append(mw.PublicPaths, login.PublicPaths()...)
	return login, nil
}

// runTLS rereads the certificates until ctx ends.
func runTLS(ctx context.Context, l *servetls.Listener) {
	if l != nil {
		go l.Run(ctx)
	}
}
