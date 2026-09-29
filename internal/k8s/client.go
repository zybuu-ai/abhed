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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Cluster is a connection to one Kubernetes API server.
type Cluster struct {
	Name      string
	Server    string
	Namespace string

	client *http.Client
	// bearer is resolved once at construction. An exec-based credential
	// (cloud CLIs use this) is re-run when it expires.
	bearer   string
	execCfg  *execConfig
	expires  time.Time
	insecure bool
}

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
	path := cfg.Kubeconfig
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("no kubeconfig configured and no home directory")
		}
		path = filepath.Join(home, ".kube", "config")
	}
	// KUBECONFIG may list several files; the first that exists wins here,
	// rather than implementing the full merge semantics for a case that has
	// not come up.
	if i := strings.IndexByte(path, os.PathListSeparator); i >= 0 {
		path = path[:i]
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig %s: %w", path, err)
	}
	kc, err := parseKubeconfig(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

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
			// binary. Running it is the documented mechanism, but it is still
			// executing a command from a config file, so it is reported at
			// startup rather than done silently.
			c.execCfg = u.Exec
			if err := c.refreshExecToken(); err != nil {
				return nil, err
			}
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
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	return c, nil
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
}

// Verification says how the server's certificate is checked, for the person
// approving a login and the line reporting it.
func (lc LoginCluster) Verification(defaultCA string) string {
	switch {
	case lc.InsecureSkipTLSVerify:
		return "TLS NOT VERIFIED (insecure_skip_tls_verify)"
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
	if !lc.InsecureSkipTLSVerify {
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
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
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

// refreshExecToken runs the credential helper named in the kubeconfig.
func (c *Cluster) refreshExecToken() error {
	if c.execCfg == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.execCfg.Command, c.execCfg.Args...)
	cmd.Env = os.Environ()
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

// Do issues a request against the API server.
func (c *Cluster) Do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	// A short-lived token that has expired produces a 401 that looks like a
	// permissions problem; refresh before that happens.
	if c.execCfg != nil && !c.expires.IsZero() && time.Now().After(c.expires.Add(-30*time.Second)) {
		if err := c.refreshExecToken(); err != nil {
			return nil, err
		}
	}

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Server+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the cluster at %s: %w", c.Server, err)
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
		return nil, fmt.Errorf("not found: %s", apiMessage(data))
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("cluster returned %s: %s", resp.Status, apiMessage(data))
	}
	return data, nil
}

// doPatch issues a PATCH with an explicit content type. Kubernetes selects the
// patch semantics from that header — apply, merge, and strategic-merge are
// three different operations behind one HTTP verb.
func (c *Cluster) doPatch(ctx context.Context, path string, body []byte, contentType string) ([]byte, error) {
	if c.execCfg != nil && !c.expires.IsZero() && time.Now().After(c.expires.Add(-30*time.Second)) {
		if err := c.refreshExecToken(); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.Server+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the cluster at %s: %w", c.Server, err)
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
