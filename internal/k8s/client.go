// Package k8s gives the agent read access to a Kubernetes cluster, and
// carefully gated write access.
//
// It speaks the API directly over HTTPS rather than importing client-go.
// That is a deliberate trade: client-go pulls roughly a hundred transitive
// dependencies, and Abhed ships as a verified air-gapped bundle where every
// one of those is something an operator has to accept. The Kubernetes API is
// REST and JSON; what client-go adds beyond that is typed structs, which an
// agent that renders results as text does not need.
//
// Credentials come from a kubeconfig, never from the model. The agent chooses
// which cluster to talk to only by naming a context the operator configured.
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
)

// Cluster is a connection to one Kubernetes API server.
type Cluster struct {
	Name      string
	Server    string
	Namespace string

	client *http.Client
	// bearer is resolved at construction, except an exec-based credential
	// (cloud CLIs use this), which is run before the first request and again
	// when it expires. tokMu guards both.
	tokMu    sync.Mutex
	bearer   string
	execCfg  *execConfig
	expires  time.Time
	insecure bool
}

// Insecure reports whether the client skips TLS verification.
func (c *Cluster) Insecure() bool { return c.insecure }

// Config selects a cluster.
type Config struct {
	// Kubeconfig path. Empty uses $KUBECONFIG, then ~/.kube/config.
	Kubeconfig string
	// Context selects one entry from the kubeconfig. Empty uses its
	// current-context.
	Context string
	// Namespace overrides the context's namespace.
	Namespace string
	// Token overrides the kubeconfig's credential.
	//
	// Cluster tokens expire, and a stale one in a kubeconfig produces a 401
	// that reads like a permissions problem. Rather than requiring the file to
	// be edited mid-session, an operator can supply a fresh token — typically
	// via ABHED_K8S_TOKEN, so it never lands in a config file the agent can
	// read.
	Token   string
	Timeout time.Duration

	// Clusters are the only servers k8s_login may send a stored token to.
	Clusters []LoginCluster
	// CAFile adds a CA bundle for a login cluster that names none of its own.
	CAFile string

	// FirstUseAll asks the first use of every kubeconfig context, as where
	// the tools are offered because the person's machine has a kubeconfig;
	// otherwise only a context whose credential helper Abhed would run asks.
	FirstUseAll bool
	// KeptLogins offers the clusters the person logged in to by pasting a
	// token, kept in their own state, beside k8s.clusters.
	KeptLogins bool
	// Secret reads a kept login's token from the secrets store; nil reads none.
	Secret func(name string) (string, error)
	// HiddenUntilLogin keeps k8s_get and k8s_apply from the model until a
	// session of this manager logs in, or a kept login exists: offered for
	// k8s_login alone, on a machine with nothing else to reach.
	HiddenUntilLogin bool
}

// ---------------------------------------------------------------- kubeconfig

// kubeconfig is the subset of the file Abhed needs. Named types rather than
// anonymous structs because the reader in kubeconfig.go builds them by hand;
// there are no struct tags, since nothing unmarshals into these.
type kubeconfig struct {
	CurrentContext string
	Contexts       []kubeContext
	Clusters       []kubeCluster
	Users          []kubeUser
}

type kubeContext struct {
	Name    string
	Cluster string
	User    string
	// Namespace is the context's default; a tool call may name another.
	Namespace string
}

type kubeCluster struct {
	Name                     string
	Server                   string
	CertificateAuthorityData string
	CertificateAuthority     string
	InsecureSkipTLSVerify    bool
}

type kubeUser struct {
	Name                  string
	Token                 string
	TokenFile             string
	ClientCertificateData string
	ClientKeyData         string
	ClientCertificate     string
	ClientKey             string
	Exec                  *execConfig
	// AuthProvider names a legacy auth-provider (gcp, azure, oidc), which
	// Abhed refuses: its refresh writes tokens back into the kubeconfig.
	AuthProvider string
	// Username is basic auth, which Abhed does not send.
	Username string
}

type execConfig struct {
	Command    string
	Args       []string
	APIVersion string
	Env        []execEnv
}

type execEnv struct{ Name, Value string }

// Open connects to the cluster named by the config.
func Open(cfg Config) (*Cluster, error) {
	kc, path, err := loadKubeconfig(cfg)
	if err != nil {
		return nil, err
	}
	return openFrom(cfg, kc, path)
}

// KubeconfigPaths are the files the config names: k8s.kubeconfig, else
// each file KUBECONFIG lists, else ~/.kube/config.
func KubeconfigPaths(cfg Config) []string {
	path := cfg.Kubeconfig
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return nil
		}
		return []string{filepath.Join(home, ".kube", "config")}
	}
	var out []string
	for _, p := range filepath.SplitList(path) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadKubeconfig reads and parses the kubeconfig the config names. Several
// files (KUBECONFIG=a:b) merge as kubectl merges them: the first file to
// name a context, cluster or user, or to set current-context, wins, and a
// listed file that does not exist is skipped.
func loadKubeconfig(cfg Config) (*kubeconfig, string, error) {
	paths := KubeconfigPaths(cfg)
	if len(paths) == 0 {
		return nil, "", fmt.Errorf("no kubeconfig configured and no home directory")
	}
	merged := &kubeconfig{}
	var read []string
	var firstErr error
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- the person's own kubeconfig, read in Abhed's process
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) || len(paths) == 1 {
				firstErr = cmpErr(firstErr, fmt.Errorf("read kubeconfig %s: %w", path, err))
			}
			continue
		}
		kc, err := parseKubeconfig(raw)
		if err != nil {
			return nil, path, fmt.Errorf("parse %s: %w", path, err)
		}
		merged.merge(kc)
		read = append(read, path)
	}
	if len(read) == 0 {
		if firstErr == nil {
			firstErr = fmt.Errorf("none of the kubeconfig files exists: %s", strings.Join(paths, ", "))
		}
		return nil, strings.Join(paths, string(os.PathListSeparator)), firstErr
	}
	return merged, strings.Join(read, string(os.PathListSeparator)), nil
}

func cmpErr(have, next error) error {
	if have != nil {
		return have
	}
	return next
}

// merge adds what o names that kc does not yet.
func (kc *kubeconfig) merge(o *kubeconfig) {
	if kc.CurrentContext == "" {
		kc.CurrentContext = o.CurrentContext
	}
	for _, c := range o.Contexts {
		if !slices.ContainsFunc(kc.Contexts, func(h kubeContext) bool { return h.Name == c.Name }) {
			kc.Contexts = append(kc.Contexts, c)
		}
	}
	for _, c := range o.Clusters {
		if !slices.ContainsFunc(kc.Clusters, func(h kubeCluster) bool { return h.Name == c.Name }) {
			kc.Clusters = append(kc.Clusters, c)
		}
	}
	for _, u := range o.Users {
		if !slices.ContainsFunc(kc.Users, func(h kubeUser) bool { return h.Name == u.Name }) {
			kc.Users = append(kc.Users, u)
		}
	}
}

func openFrom(cfg Config, kc *kubeconfig, path string) (*Cluster, error) {
	var err error
	ctxName := cfg.Context
	if ctxName == "" {
		ctxName = kc.CurrentContext
	}
	if ctxName == "" {
		return nil, fmt.Errorf("no context selected and %s has no current-context", path)
	}

	var clusterName, userName, namespace string
	found := false
	for _, c := range kc.Contexts {
		if c.Name == ctxName {
			clusterName, userName, namespace = c.Cluster, c.User, c.Namespace
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("context %q not found in %s (available: %s)",
			ctxName, path, strings.Join(contextNames(kc), ", "))
	}
	if cfg.Namespace != "" {
		namespace = cfg.Namespace
	}
	if namespace == "" {
		namespace = "default"
	}

	c := &Cluster{Name: ctxName, Namespace: namespace}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	for _, cl := range kc.Clusters {
		if cl.Name != clusterName {
			continue
		}
		c.Server = strings.TrimSuffix(cl.Server, "/")
		c.insecure = cl.InsecureSkipTLSVerify
		tlsCfg.InsecureSkipVerify = cl.InsecureSkipTLSVerify

		var caPEM []byte
		if cl.CertificateAuthorityData != "" {
			caPEM, err = base64.StdEncoding.DecodeString(cl.CertificateAuthorityData)
			if err != nil {
				return nil, fmt.Errorf("cluster %s: bad certificate-authority-data: %w", cl.Name, err)
			}
		} else if cl.CertificateAuthority != "" {
			caPEM, err = os.ReadFile(cl.CertificateAuthority)
			if err != nil {
				return nil, fmt.Errorf("cluster %s: read CA: %w", cl.Name, err)
			}
		}
		if len(caPEM) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				return nil, fmt.Errorf("cluster %s: CA bundle contains no usable certificate", cl.Name)
			}
			tlsCfg.RootCAs = pool
		}
		break
	}
	if c.Server == "" {
		return nil, fmt.Errorf("cluster %q not found in %s", clusterName, path)
	}

	// An explicitly supplied token wins over the kubeconfig, so a fresh one
	// can be used without editing the file.
	if cfg.Token != "" {
		c.bearer = cfg.Token
	}
	for _, u := range kc.Users {
		if cfg.Token != "" {
			break
		}
		if u.Name != userName {
			continue
		}
		if why := refusedAuth(u); why != "" {
			return nil, fmt.Errorf("context %q: %s", ctxName, why)
		}
		switch {
		case u.Token != "":
			c.bearer = u.Token
		case u.TokenFile != "":
			tok, err := os.ReadFile(u.TokenFile)
			if err != nil {
				return nil, fmt.Errorf("user %s: read tokenFile: %w", u.Name, err)
			}
			c.bearer = strings.TrimSpace(string(tok))
		case u.Exec != nil:
			// Cloud providers hand out short-lived tokens through a helper
			// binary. It runs when the first request is sent, not here: an
			// approval opens the client to name its server, and a call that
			// is then refused must run nothing.
			c.execCfg = u.Exec
		}
		if cert, key := clientCert(u.ClientCertificateData, u.ClientCertificate,
			u.ClientKeyData, u.ClientKey); cert != nil {
			_ = key
			tlsCfg.Certificates = []tls.Certificate{*cert}
		}
		break
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	c.client = &http.Client{
		Timeout:   timeout,
		Transport: newTransport(tlsCfg),
	}
	return c, nil
}

// refusedAuth is why Abhed will not use a kubeconfig user, or "".
func refusedAuth(u kubeUser) string {
	switch {
	case u.AuthProvider != "":
		return fmt.Sprintf("it signs in with the auth-provider %q, which Abhed does not use: that flow "+
			"refreshes tokens by writing them back into your kubeconfig, and Abhed never writes to it. "+
			"Switch the context to its exec credential plugin (such as kubelogin, gke-gcloud-auth-plugin "+
			"or kubelogin for Azure), or log in with a token through k8s_login", u.AuthProvider)
	case u.Username != "" && u.Token == "" && u.TokenFile == "" && u.Exec == nil &&
		u.ClientCertificateData == "" && u.ClientCertificate == "":
		return "it signs in with a user name and password (basic auth), which Abhed does not send; " +
			"use a token, a client certificate or an exec credential plugin instead"
	}
	return ""
}

// LoginCluster is a cluster the operator lets k8s_login reach. The model
// names one; it never supplies the server a stored token is sent to.
type LoginCluster struct {
	Name   string
	Server string
	// CAFile verifies the server; empty uses Config.CAFile, then the system roots.
	CAFile string
	// InsecureSkipTLSVerify turns verification off. Operator config only, and
	// warned about, since the token then goes to whoever answers.
	InsecureSkipTLSVerify bool
	// CAPEM, when set, is the only certificate the server is verified
	// against: the one the person approved when they logged in by pasting a
	// token. CAFingerprint is its SHA-256, as they were shown it.
	CAPEM         []byte
	CAFingerprint string
}

// Verification says how the server's certificate is checked, for the person
// approving a login and the line reporting it.
func (lc LoginCluster) Verification(defaultCA string) string {
	switch {
	case lc.InsecureSkipTLSVerify:
		return "TLS NOT VERIFIED (insecure_skip_tls_verify)"
	case len(lc.CAPEM) > 0 && lc.CAFingerprint != "":
		return "TLS verified against the certificate you approved (SHA-256 " + lc.CAFingerprint + ")"
	case len(lc.CAPEM) > 0:
		return "TLS verified against the cluster's CA from your kubeconfig"
	case lc.CAFile != "":
		return "TLS verified against " + lc.CAFile
	case defaultCA != "":
		return "TLS verified against " + defaultCA
	}
	return "TLS verified against the system roots"
}

// OpenLogin connects to an operator-declared cluster with a token obtained at
// run time. TLS is verified against the system roots plus the cluster's CA
// bundle, or k8s.ca_file; only the cluster's own insecure flag turns it off.
func OpenLogin(lc LoginCluster, defaultCA, token, namespace string) (*Cluster, error) {
	if !strings.HasPrefix(lc.Server, "https://") {
		return nil, fmt.Errorf("cluster %s: server must be an https:// URL, got %q", lc.Name, lc.Server)
	}
	if token == "" {
		return nil, fmt.Errorf("token is required")
	}
	if namespace == "" {
		namespace = "default"
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: lc.InsecureSkipTLSVerify} // #nosec G402 -- the operator's opt-out for one cluster, warned at startup
	if !lc.InsecureSkipTLSVerify && len(lc.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(lc.CAPEM) {
			return nil, fmt.Errorf("cluster %s: the kept certificate is not usable", lc.Name)
		}
		tlsCfg.RootCAs = pool
	} else if !lc.InsecureSkipTLSVerify {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		ca := lc.CAFile
		if ca == "" {
			ca = defaultCA
		}
		if ca != "" {
			pem, err := os.ReadFile(ca) // #nosec G304 -- the CA bundle the operator configured
			if err != nil {
				return nil, fmt.Errorf("cluster %s: read CA: %w", lc.Name, err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("cluster %s: CA bundle %s contains no usable certificate", lc.Name, ca)
			}
		}
		tlsCfg.RootCAs = pool
	}
	return &Cluster{
		Name: lc.Name, Server: strings.TrimSuffix(lc.Server, "/"),
		Namespace: namespace, bearer: token, insecure: lc.InsecureSkipTLSVerify,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: newTransport(tlsCfg),
		},
	}, nil
}

// newTransport keeps idle connections for a while only: a session's clients
// outlive its last call until the session goes, and should not hold sockets.
// It is judged by the egress allowlist when one is in force, as a command's
// kubectl through the proxy would be.
func newTransport(tlsCfg *tls.Config) http.RoundTripper {
	return &egress.Transport{Kind: egress.KindK8s, Base: &http.Transport{TLSClientConfig: tlsCfg,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}}
}

func contextNames(kc *kubeconfig) []string {
	out := make([]string, 0, len(kc.Contexts))
	for _, c := range kc.Contexts {
		out = append(out, c.Name)
	}
	return out
}

func clientCert(certData, certFile, keyData, keyFile string) (*tls.Certificate, error) {
	var certPEM, keyPEM []byte
	var err error
	if certData != "" {
		certPEM, err = base64.StdEncoding.DecodeString(certData)
	} else if certFile != "" {
		certPEM, err = os.ReadFile(certFile)
	}
	if err != nil || len(certPEM) == 0 {
		return nil, err
	}
	if keyData != "" {
		keyPEM, err = base64.StdEncoding.DecodeString(keyData)
	} else if keyFile != "" {
		keyPEM, err = os.ReadFile(keyFile)
	}
	if err != nil || len(keyPEM) == 0 {
		return nil, err
	}
	if block, _ := pem.Decode(certPEM); block == nil {
		return nil, fmt.Errorf("client certificate is not PEM")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &pair, nil
}

// helperEnv is a credential helper's environment: what a program needs to
// run, and what cloud sign-in helpers read (their own variables, proxies,
// certificate bundles), never Abhed's own keys such as ABHED_API_KEY or a
// model provider's. The kubeconfig's exec env is added after.
func helperEnv() []string {
	names := map[string]bool{}
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TZ",
		"KUBECONFIG", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME",
		"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		names[k] = true
	}
	prefixes := []string{"AWS_", "AZURE_", "GOOGLE_", "CLOUDSDK_", "GCE_", "OCI_", "IBMCLOUD_", "DIGITALOCEAN_", "KUBE"}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		keep := names[k]
		for _, p := range prefixes {
			keep = keep || strings.HasPrefix(k, p)
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}

// refreshExecToken runs the credential helper named in the kubeconfig.
func (c *Cluster) refreshExecToken() error {
	if c.execCfg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.execCfg.Command, c.execCfg.Args...)
	cmd.Env = helperEnv()
	for _, e := range c.execCfg.Env {
		cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
	}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("credential helper %q failed: %w "+
			"(is it on PATH and are you logged in?)", c.execCfg.Command, err)
	}
	var cred struct {
		Status struct {
			Token               string `json:"token"`
			ExpirationTimestamp string `json:"expirationTimestamp"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &cred); err != nil {
		return fmt.Errorf("credential helper %q returned unparseable output: %w",
			c.execCfg.Command, err)
	}
	if cred.Status.Token == "" {
		return fmt.Errorf("credential helper %q returned no token", c.execCfg.Command)
	}
	c.bearer = cred.Status.Token
	if cred.Status.ExpirationTimestamp != "" {
		if t, err := time.Parse(time.RFC3339, cred.Status.ExpirationTimestamp); err == nil {
			c.expires = t
		}
	}
	return nil
}

// ---------------------------------------------------------------- requests

// unreachable reports a failed request by what went wrong, without the
// request URL, whose query may carry a token the kubeconfig wrote there.
func (c *Cluster) unreachable(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("cannot reach the cluster at %s: %w", displayURL(c.Server), err)
}

// credential returns the bearer to send, running the exec helper first when
// there is none yet or it is about to expire.
func (c *Cluster) credential() (string, error) {
	c.tokMu.Lock()
	defer c.tokMu.Unlock()
	if c.execCfg != nil && (c.bearer == "" ||
		!c.expires.IsZero() && time.Now().After(c.expires.Add(-30*time.Second))) {
		if err := c.refreshExecToken(); err != nil {
			return "", err
		}
	}
	return c.bearer, nil
}

// needsHelper reports whether the next request would run the exec
// credential helper first.
func (c *Cluster) needsHelper() bool {
	c.tokMu.Lock()
	defer c.tokMu.Unlock()
	return c.execCfg != nil && (c.bearer == "" ||
		!c.expires.IsZero() && time.Now().After(c.expires.Add(-30*time.Second)))
}

// Do issues a request against the API server.
func (c *Cluster) Do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	// A short-lived token that has expired produces a 401 that looks like a
	// permissions problem; refresh before that happens.
	bearer, err := c.credential()
	if err != nil {
		return nil, err
	}

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, rdr)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, c.unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// Name the fix. "Re-authenticate" alone left a user watching the agent
		// fail with no idea that a token in their kubeconfig had expired.
		return nil, fmt.Errorf("the cluster rejected the credentials (401): the token " +
			"for this context has expired. Re-authenticate (oc login / gcloud / az) to " +
			"refresh the kubeconfig, or set ABHED_K8S_TOKEN to a fresh token and restart " +
			"Abhed. Do not retry — it will fail identically")
	case resp.StatusCode == http.StatusForbidden:
		// RBAC denials carry a precise message; surfacing it saves the agent
		// guessing at which verb or resource it lacks.
		return nil, fmt.Errorf("forbidden by RBAC: %s", apiMessage(data))
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", errNotFound, apiMessage(data))
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("cluster returned %s: %s", resp.Status, apiMessage(data))
	}
	return data, nil
}

// doPatch issues a PATCH with an explicit content type. Kubernetes selects the
// patch semantics from that header — apply, merge, and strategic-merge are
// three different operations behind one HTTP verb.
func (c *Cluster) doPatch(ctx context.Context, path string, body []byte, contentType string) ([]byte, error) {
	bearer, err := c.credential()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.Server+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, c.unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("the cluster rejected the credentials (401)")
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("forbidden by RBAC: %s", apiMessage(data))
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("cluster returned %s: %s", resp.Status, apiMessage(data))
	}
	return data, nil
}

// errNotFound is a 404 from the API server.
var errNotFound = errors.New("not found")

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// apiMessage pulls the human-readable part out of a Kubernetes Status object.
func apiMessage(data []byte) string {
	var status struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &status); err == nil && status.Message != "" {
		return status.Message
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// InsecureContext reports whether the kubeconfig the config names skips TLS
// verification for the cluster of the context in use, and names that
// context. It only reads the file: nothing is run and nothing is reached.
func InsecureContext(cfg Config) (string, bool) {
	kc, _, err := loadKubeconfig(cfg)
	if err != nil {
		return "", false
	}
	ctxName := cfg.Context
	if ctxName == "" {
		ctxName = kc.CurrentContext
	}
	for _, c := range kc.Contexts {
		if c.Name != ctxName {
			continue
		}
		for _, cl := range kc.Clusters {
			if cl.Name == c.Cluster {
				return ctxName, cl.InsecureSkipTLSVerify
			}
		}
	}
	return ctxName, false
}

// ContextInfo describes one kubeconfig context for the person approving its
// first use and for the grant that keeps it: where it goes and how it signs
// in, never with what.
type ContextInfo struct {
	Name, Server, Namespace string
	// Auth says how it signs in: "a token", "a client certificate", …
	Auth string
	// Exec is the credential helper's command line, when it runs one.
	Exec     string
	Insecure bool
	// Refused is why Abhed will not use it, or "".
	Refused string
	// Identity sums what a grant for it covers; see machinegrant.Identity.
	Identity string
}

// Contexts lists the kubeconfig's contexts, its current context first.
// It only reads the files: nothing is run and nothing is reached.
func Contexts(cfg Config) (list []ContextInfo, current string, err error) {
	kc, _, err := loadKubeconfig(cfg)
	if err != nil {
		return nil, "", err
	}
	for _, c := range kc.Contexts {
		if info, ok := contextInfo(kc, c.Name); ok {
			list = append(list, info)
		}
	}
	slices.SortStableFunc(list, func(a, b ContextInfo) int {
		switch {
		case a.Name == kc.CurrentContext:
			return -1
		case b.Name == kc.CurrentContext:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return list, kc.CurrentContext, nil
}

// contextInfo describes the context name names in kc.
func contextInfo(kc *kubeconfig, name string) (ContextInfo, bool) {
	var ctx *kubeContext
	for i := range kc.Contexts {
		if kc.Contexts[i].Name == name {
			ctx = &kc.Contexts[i]
			break
		}
	}
	if ctx == nil {
		return ContextInfo{}, false
	}
	info := ContextInfo{Name: name, Namespace: ctx.Namespace}
	ca := ""
	for _, cl := range kc.Clusters {
		if cl.Name == ctx.Cluster {
			info.Server, info.Insecure = strings.TrimSuffix(cl.Server, "/"), cl.InsecureSkipTLSVerify
			ca = cl.CertificateAuthorityData + "\x00" + cl.CertificateAuthority
			if cl.CertificateAuthority != "" {
				// The file's content, so a CA swapped in at the same path asks again.
				if b, err := os.ReadFile(cl.CertificateAuthority); err == nil { // #nosec G304 -- the CA the person's kubeconfig names
					ca += "\x00" + machinegrant.Identity(string(b))
				}
			}
			break
		}
	}
	info.Auth = "no credential"
	for _, u := range kc.Users {
		if u.Name != ctx.User {
			continue
		}
		info.Refused = refusedAuth(u)
		switch {
		case u.AuthProvider != "":
			info.Auth = "the auth-provider " + u.AuthProvider
		case u.Token != "":
			info.Auth = "the kubeconfig's token"
		case u.TokenFile != "":
			info.Auth = "the token in " + u.TokenFile
		case u.Exec != nil:
			info.Exec = strings.TrimSpace(u.Exec.Command + " " + strings.Join(u.Exec.Args, " "))
			info.Auth = "a token from the credential helper `" + info.Exec + "`, which Abhed runs on your machine"
		case u.ClientCertificateData != "" || u.ClientCertificate != "":
			info.Auth = "the kubeconfig's client certificate"
		case u.Username != "":
			info.Auth = "a user name and password"
		}
		break
	}
	execEnv := ""
	for _, u := range kc.Users {
		if u.Name == ctx.User && u.Exec != nil {
			for _, e := range u.Exec.Env {
				execEnv += e.Name + "=" + e.Value + "\x00"
			}
			// The program PATH finds now, so another one found first asks again.
			if p, err := exec.LookPath(u.Exec.Command); err == nil {
				execEnv += "path=" + p
			}
		}
	}
	info.Identity = machinegrant.Identity(name, info.Server, ca, strconv.FormatBool(info.Insecure), ctx.User, info.Auth, info.Exec, execEnv)
	return info, true
}
