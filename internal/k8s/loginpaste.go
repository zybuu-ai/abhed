package k8s

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A token the person pasted (oc login --token=… --server=…) becomes a kept
// login with one approval: the token is stored as a secret, the cluster is
// kept for them with the server they pasted it with, and the session logs
// in. The model names the paste by its id and chooses a short name; it
// never sees the token and cannot change the server it goes to.
//
// The server's certificate is checked before anyone is asked: one a public
// CA vouches for is verified as any site is; otherwise the CA the person's
// kubeconfig holds for that server; otherwise the certificate the server
// presents now is shown by its fingerprint, and approving pins it, as
// `oc login` asks to. The token is sent only after, over that check.

// pastePlan is what a pasted login will do, read before it is approved.
type pastePlan struct {
	server, cluster, storeAs, namespace string
	caPEM                               []byte
	fingerprint, subject                string
	// replaces marks a new token for a cluster kept already; write that the
	// token is written to the store; created that the paste flow made the
	// secret, so a later paste may replace it.
	replaces, write, created bool
	how                      string // how TLS will be verified, for the person
	kind                     string
}

type pastePlansKey struct{ m *Manager }

type pastePlans struct {
	mu    sync.Mutex
	plans map[string]pastePlan
}

func (t LoginTool) plans(sess *tools.Session) *pastePlans {
	p, _ := sess.Scoped(pastePlansKey{t.M}, func() any { return &pastePlans{plans: map[string]pastePlan{}} }).(*pastePlans)
	return p
}

func planKey(a loginArgs) string {
	return a.Pasted + "\x00" + a.Cluster + "\x00" + a.StoreAs + "\x00" + a.Namespace
}

// storeName is the secret a pasted token is stored under.
func storeName(a loginArgs) string {
	if a.StoreAs != "" {
		return strings.TrimSpace(a.StoreAs)
	}
	return "K8S_" + strings.ToUpper(nonIdent.ReplaceAllString(a.Cluster, "_")) + "_TOKEN"
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9]+`)

// clusterNameRe is a kept cluster's name: one segment rules can name.
var clusterNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// planPasted checks a pasted login and reads the server's certificate.
func (t LoginTool) planPasted(ctx context.Context, sess *tools.Session, a loginArgs) error {
	if t.M == nil || sess == nil {
		return fmt.Errorf("k8s_login needs a session to use a pasted token")
	}
	if t.M.cfg.PastedOff != "" {
		return fmt.Errorf("a pasted token cannot be used on this machine: %s. Tell the user so; do not ask them to paste it again", t.M.cfg.PastedOff)
	}
	if !t.M.cfg.KeptLogins && !t.M.cfg.SessionLogins {
		return fmt.Errorf("a pasted token can be used only where Abhed runs on the user's own machine; here, " +
			"ask the user to store it with `abhed secret set NAME` and pass that NAME as token_secret")
	}
	p, ok := tools.PastedValue(sess, a.Pasted)
	if !ok {
		return fmt.Errorf("no pasted credential %q is held in this session; Abhed names each one it withholds (P1, P2, …)", a.Pasted)
	}
	if p.Server == "" {
		return fmt.Errorf("the credential pasted as %s came without a server, so Abhed does not know where it may go. "+
			"Ask the user to paste the whole `oc login --token=… --server=https://…` command", a.Pasted)
	}
	if !clusterNameRe.MatchString(a.Cluster) {
		return fmt.Errorf("cluster %q: choose a short name of letters, digits, '.', '_' and '-' to keep it under", a.Cluster)
	}
	if slices.ContainsFunc(t.M.cfg.Clusters, func(c LoginCluster) bool { return strings.EqualFold(c.Name, a.Cluster) }) {
		return fmt.Errorf("cluster %q is declared in k8s.clusters, which wins; keep the pasted one under another name", a.Cluster)
	}
	if t.M.isContext(a.Cluster) {
		return fmt.Errorf("%q is a kubeconfig context on this machine; keep the pasted cluster under another name", a.Cluster)
	}
	value, err := t.pastedValue(p)
	if err != nil {
		return err
	}
	// The same token kept already: nothing to do, and nothing is sent.
	for cluster, kl := range machinegrant.Logins() {
		if kl.Server != strings.TrimSuffix(p.Server, "/") {
			continue
		}
		same := p.SecretRef != "" && kl.Secret == p.SecretRef
		if !same && t.Secret != nil {
			v, err := t.Secret(kl.Secret)
			same = err == nil && strings.TrimSpace(v) == value
		}
		if same {
			return fmt.Errorf("already kept: this is the token kept for cluster %s at %s (secret %s), so nothing was "+
				"changed. Name it as cluster %q in k8s_get and k8s_apply", cluster, displayURL(kl.Server), kl.Secret, cluster)
		}
	}
	target, err := t.pasteTarget(p, a, value)
	if err != nil {
		return err
	}
	name, replaces := target.name, target.replaces
	u, err := url.Parse(p.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("the server pasted with %s is not an https:// URL with a host, so no token is sent to it", a.Pasted)
	}
	plan := pastePlan{server: strings.TrimSuffix(p.Server, "/"), cluster: a.Cluster, storeAs: name,
		namespace: orDefault(a.Namespace, p.Namespace), kind: p.Kind, replaces: replaces,
		write: target.write, created: target.created}
	if err := plan.readTLS(ctx, t.M); err != nil {
		return err
	}
	ps := t.plans(sess)
	if ps == nil {
		return fmt.Errorf("the session has ended")
	}
	ps.mu.Lock()
	ps.plans[planKey(a)] = plan
	ps.mu.Unlock()
	return nil
}

func orDefault(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// readTLS decides how the server will be verified: by the system roots, by
// the CA the kubeconfig holds for the same server, or by pinning what it
// presents now. Nothing but a TLS handshake is sent.
func (p *pastePlan) readTLS(ctx context.Context, m *Manager) error {
	u, _ := url.Parse(p.server)
	host := u.Hostname()
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(host, "443")
	}
	chain, err := presentedChain(ctx, addr, host)
	if err != nil {
		return fmt.Errorf("cannot read the certificate of %s: %v; nothing was sent", displayURL(p.server), err)
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	verify := func(roots *x509.CertPool) bool {
		_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: host})
		return err == nil
	}
	if sys, err := x509.SystemCertPool(); err == nil && sys != nil && verify(sys) {
		p.how = "TLS verified against the system roots"
		return nil
	}
	if ca := kubeconfigCA(m, p.server); len(ca) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(ca) && verify(pool) {
			p.caPEM, p.how = ca, "TLS verified against the CA your kubeconfig holds for this server"
			return nil
		}
	}
	top := chain[len(chain)-1]
	pool := x509.NewCertPool()
	pool.AddCert(top)
	if !verify(pool) {
		return fmt.Errorf("the certificate %s presents does not name %s, so no token is sent to it", displayURL(p.server), host)
	}
	sum := sha256.Sum256(top.Raw)
	p.fingerprint, p.subject = colonHex(sum[:]), top.Subject.String()
	p.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: top.Raw})
	p.how = fmt.Sprintf("no public CA vouches for this server; approving pins the certificate it presents now, %q, "+
		"SHA-256 %s: check it matches your cluster's before you approve", top.Subject.CommonName, p.fingerprint)
	return nil
}

// presentedChain is what the server at addr presents, read without trusting it.
func presentedChain(ctx context.Context, addr, host string) ([]*x509.Certificate, error) {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := egress.Dial(dctx, egress.KindK8s, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = raw.Close() }()
	// Read only: no credential is sent on this connection, and the chain is
	// verified by the caller before any is.
	conn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) // #nosec G402 -- reads the chain to show and verify it; nothing is sent
	if err := conn.HandshakeContext(dctx); err != nil {
		return nil, err
	}
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		return nil, fmt.Errorf("the server presented no certificate")
	}
	return chain, nil
}

// kubeconfigCA is the CA the person's kubeconfig holds for server, if any.
func kubeconfigCA(m *Manager, server string) []byte {
	kc, _, err := loadKubeconfig(m.cfg)
	if err != nil {
		return nil
	}
	for _, cl := range kc.Clusters {
		if strings.TrimSuffix(cl.Server, "/") != server || cl.CertificateAuthorityData == "" {
			continue
		}
		if b, err := base64.StdEncoding.DecodeString(cl.CertificateAuthorityData); err == nil {
			return b
		}
	}
	return nil
}

func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02X", c)
	}
	return strings.Join(parts, ":")
}

func (t LoginTool) plan(sess *tools.Session, a loginArgs) (pastePlan, bool) {
	ps := t.plans(sess)
	if ps == nil {
		return pastePlan{}, false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p, ok := ps.plans[planKey(a)]
	return p, ok
}

// pastedTarget says what the one approval does.
func (t LoginTool) pastedTarget(sess *tools.Session, a loginArgs) string {
	p, ok := t.plan(sess, a)
	if !ok {
		return ""
	}
	if !t.M.cfg.KeptLogins {
		return fmt.Sprintf("logs this session in to cluster %s at %s with the token you pasted (%s, %s), for this "+
			"session only: as a guest, nothing is kept, and the token goes when you quit; %s. The token is never "+
			"shown to the agent", p.cluster, displayURL(p.server), a.Pasted, p.kind, p.how)
	}
	if p.replaces {
		return fmt.Sprintf("replaces the token kept for cluster %s with the one you pasted (%s, %s), in secret %s, "+
			"and keeps the cluster at %s for you; %s. The token is never shown to the agent", p.cluster, a.Pasted, p.kind,
			p.storeAs, displayURL(p.server), p.how)
	}
	return fmt.Sprintf("stores the token you pasted (%s, %s) as secret %s, keeps cluster %s at %s for you "+
		"(in your own state, not the workspace) so k8s_login may send that secret to that server only, and logs "+
		"this session in; %s. The token is never shown to the agent", a.Pasted, p.kind, p.storeAs, p.cluster,
		displayURL(p.server), p.how)
}

// runPasted verifies the pasted token on the server, then stores and keeps it.
func (t LoginTool) runPasted(ctx context.Context, sess *tools.Session, a loginArgs) tools.Result {
	p, ok := t.plan(sess, a)
	if !ok {
		return errf("This pasted login was not checked before it was approved; make the call again.")
	}
	pasted, ok := tools.PastedValue(sess, a.Pasted)
	if !ok || strings.TrimSuffix(pasted.Server, "/") != p.server {
		return errf("The credential pasted as %s is no longer held in this session; ask the user to paste it again.", a.Pasted)
	}
	value, err := t.pastedValue(pasted)
	if err != nil {
		return errf("%v", err)
	}
	lc := LoginCluster{Name: p.cluster, Server: p.server, CAPEM: p.caPEM, CAFingerprint: p.fingerprint}
	c, err := OpenLogin(lc, "", value, orDefaultNS(p.namespace, t.M.cfg.Namespace))
	if err != nil {
		return errf("%v", err)
	}
	defer c.client.CloseIdleConnections()
	if _, err := c.Do(ctx, "GET", "/version", nil); err != nil {
		return errf("Could not authenticate to %s with the pasted token: %v. Nothing was stored.", displayURL(p.server), err)
	}
	if !t.M.cfg.KeptLogins {
		// A guest: the session logs in, and nothing is stored or kept.
		if err := t.M.login(sess, sessionCred{token: value, cluster: lc, namespace: p.namespace}); err != nil {
			return errf("%v", err)
		}
		tools.DropPasted(sess, a.Pasted)
		return tools.Result{Content: fmt.Sprintf("Logged in to cluster %s at %s (namespace %s, %s) for this session only: "+
			"as a guest, nothing is stored or kept. Name it as cluster %q in k8s_get and k8s_apply.",
			p.cluster, displayURL(p.server), c.Namespace, lc.Verification(""), p.cluster)}
	}
	if p.write {
		if err := t.Store.Set(p.storeAs, value); err != nil {
			return errf("The token works, but it could not be stored as %s: %v. Nothing was kept.", p.storeAs, err)
		}
	}
	grant := machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: p.cluster,
		Identity: machinegrant.Identity(p.server, string(p.caPEM), p.storeAs),
		What:     "cluster " + p.cluster + " at " + displayURL(p.server) + " with secret " + p.storeAs,
		Login: &machinegrant.Login{Server: p.server, CAPEM: string(p.caPEM), CAFingerprint: p.fingerprint,
			Secret: p.storeAs, Namespace: p.namespace, Created: p.created}}
	kept := "kept for your later sessions"
	if err := machinegrant.Keep(grant); err != nil {
		kept = "NOT kept for later sessions (" + err.Error() + "); it holds for this session"
	}
	if err := t.M.login(sess, sessionCred{token: value, cluster: lc, namespace: p.namespace}); err != nil {
		return errf("%v", err)
	}
	tools.DropPasted(sess, a.Pasted)
	return tools.Result{Content: fmt.Sprintf(
		"Logged in to cluster %s at %s (namespace %s, %s). The token is stored as secret %s and the cluster is %s. "+
			"Name it as cluster %q in k8s_get and k8s_apply; later sessions sign in to it without a paste.",
		p.cluster, displayURL(p.server), c.Namespace, lc.Verification(""), p.storeAs, kept, p.cluster)}
}

// PinnedCertificate names the certificate a pasted login will pin, for the
// approval to show apart from its text; none when a CA vouches for the server.
func (t LoginTool) PinnedCertificate(sess *tools.Session, raw json.RawMessage) (string, string, bool) {
	var a loginArgs
	if json.Unmarshal(raw, &a) != nil || a.Pasted == "" {
		return "", "", false
	}
	p, ok := t.plan(sess, a)
	if !ok || p.fingerprint == "" {
		return "", "", false
	}
	return p.fingerprint, p.subject, true
}

// pastedValue is the pasted token: as held, or the stored secret it already is.
func (t LoginTool) pastedValue(p tools.Pasted) (string, error) {
	if p.SecretRef == "" {
		return p.Value, nil
	}
	if t.Secret == nil {
		return "", fmt.Errorf("no secrets store is available here, so the stored secret %s cannot be read", p.SecretRef)
	}
	v, err := t.Secret(p.SecretRef)
	if err != nil {
		return "", fmt.Errorf("the stored secret %s the login names cannot be read: %v", p.SecretRef, err)
	}
	return strings.TrimSpace(v), nil
}

// pasteTarget is where a pasted login's token is kept.
type pasteTarget struct {
	name                     string
	write, replaces, created bool
}

// pasteTarget decides the secret a pasted login binds: the stored secret it
// already is; else, for a cluster kept already, the secret the paste flow made
// for that login alone, replaced; else store_as or K8S_<CLUSTER>_TOKEN, which
// must be new or hold this very value. A secret another login binds, or one
// that held another value, is never overwritten.
func (t LoginTool) pasteTarget(p tools.Pasted, a loginArgs, value string) (pasteTarget, error) {
	if p.SecretRef != "" {
		return pasteTarget{name: p.SecretRef}, nil
	}
	if t.Store == nil {
		return pasteTarget{}, fmt.Errorf("no secrets store is available here, so a pasted token cannot be kept")
	}
	logins := machinegrant.Logins()
	boundElsewhere := func(name string) string {
		for cluster, kl := range logins {
			if kl.Secret == name && cluster != a.Cluster {
				return cluster
			}
		}
		return ""
	}
	if kept, ok := t.M.keptLogin(a.Cluster); ok && a.StoreAs == "" && kept.Created && boundElsewhere(kept.Secret) == "" {
		return pasteTarget{name: kept.Secret, write: true, replaces: true, created: true}, nil
	}
	name := storeName(a)
	if !secrets.ValidName(name) || secrets.Held(name) {
		return pasteTarget{}, fmt.Errorf("store_as %q is not a secret name (upper-case letters, digits and '_')", name)
	}
	if other := boundElsewhere(name); other != "" {
		return pasteTarget{}, fmt.Errorf("the secret %s is the one cluster %s is kept with; choose another store_as", name, other)
	}
	exists := false
	if names, err := t.Store.Names(); err == nil && slices.Contains(names, name) {
		exists = true
		if v, err := t.Store.Value(name); err != nil || strings.TrimSpace(v) != value {
			return pasteTarget{}, fmt.Errorf("a secret named %s already holds another value; choose another store_as", name)
		}
	}
	_, kept := t.M.keptLogin(a.Cluster)
	return pasteTarget{name: name, write: !exists, replaces: kept, created: !exists}, nil
}
