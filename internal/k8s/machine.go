package k8s

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// The person's own machine setup: kubeconfig contexts used as they are,
// with each first use asked once, and clusters they logged in to by pasting
// a token, kept for them. Every credential is read in Abhed's own process;
// the model names a context or a cluster, never a server or a token.

// loginCluster is the cluster k8s_login and the session's logins name:
// one in k8s.clusters, which wins a clash, else one the person kept.
func (m *Manager) loginCluster(name string) (LoginCluster, bool) {
	for _, c := range m.cfg.Clusters {
		if c.Name == name {
			return c, true
		}
	}
	if kl, ok := m.keptLogin(name); ok {
		return keptCluster(name, kl), true
	}
	return LoginCluster{}, false
}

// keptLogin is the person's kept login named name, unless k8s.clusters
// declares that name, which then wins.
func (m *Manager) keptLogin(name string) (machinegrant.Login, bool) {
	if !m.cfg.KeptLogins || name == "" {
		return machinegrant.Login{}, false
	}
	if slices.ContainsFunc(m.cfg.Clusters, func(c LoginCluster) bool { return c.Name == name }) {
		return machinegrant.Login{}, false
	}
	kl, ok := machinegrant.Logins()[name]
	return kl, ok
}

func keptCluster(name string, kl machinegrant.Login) LoginCluster {
	return LoginCluster{Name: name, Server: kl.Server, CAPEM: []byte(kl.CAPEM), CAFingerprint: kl.CAFingerprint}
}

// loginNames are the clusters k8s_login may name, sorted, the configured first.
func (m *Manager) loginNames() []string {
	var out []string
	for _, c := range m.cfg.Clusters {
		out = append(out, c.Name)
	}
	for _, name := range m.keptNames() {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// isContext reports a context the kubeconfig names.
func (m *Manager) isContext(name string) bool {
	kc, _, err := loadKubeconfig(m.cfg)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(kc.Contexts, func(c kubeContext) bool { return c.Name == name })
}

// contextNow describes the context a call names, "" for the current one,
// as the kubeconfig reads now.
func (m *Manager) contextNow(name string) (ContextInfo, bool) {
	kc, _, err := loadKubeconfig(m.cfg)
	if err != nil {
		return ContextInfo{}, false
	}
	if name == "" {
		name = m.cfg.Context
	}
	if name == "" {
		name = kc.CurrentContext
	}
	return contextInfo(kc, name)
}

// contextList names the kubeconfig's contexts for the tools' descriptions.
func (m *Manager) contextList() string {
	if m == nil {
		return ""
	}
	list, current, err := Contexts(m.cfg)
	if err != nil || len(list) == 0 {
		return ""
	}
	names := make([]string, 0, len(list))
	for _, c := range list {
		n := c.Name
		if n == current {
			n += " (current)"
		}
		if c.Refused != "" {
			n += " (not usable: " + strings.SplitN(c.Refused, ",", 2)[0] + ")"
		}
		names = append(names, n)
	}
	if len(names) > 20 {
		names = append(names[:20], fmt.Sprintf("and %d more", len(names)-20))
	}
	return " Kubeconfig contexts on this machine: " + strings.Join(names, ", ") + ". Name one as context " +
		"(or as cluster); Abhed signs in with its credential, which you never see."
}

// firstUse is what a k8s_get or k8s_apply call needs before it reaches a
// kubeconfig context: the person's yes, once. A declared cluster or a kept
// login is the operator's or the person's already, and needs none.
func (m *Manager) firstUse(sess *tools.Session, raw json.RawMessage) (tools.FirstUse, bool) {
	if m == nil {
		return tools.FirstUse{}, false
	}
	var a struct {
		Cluster string `json:"cluster"`
		Context string `json:"context"`
	}
	if json.Unmarshal(raw, &a) != nil || a.Cluster != "" {
		return tools.FirstUse{}, false
	}
	info, ok := m.contextNow(a.Context)
	if !ok || info.Refused != "" {
		return tools.FirstUse{}, false // the call is refused, and says why
	}
	if !m.cfg.FirstUseAll && info.Exec == "" {
		return tools.FirstUse{}, false
	}
	if machinegrant.Kept(machinegrant.KindK8sContext, info.Name, info.Identity) {
		return tools.FirstUse{}, false
	}
	why := fmt.Sprintf("first use of kubeconfig context %s at %s, signing in with %s; the credential stays in "+
		"Abhed and never reaches the agent, the sandbox or the record", info.Name, displayURL(info.Server), info.Auth)
	if info.Exec != "" {
		why += fmt.Sprintf(". Approving lets Abhed run `%s` on your machine, now and whenever its token expires", info.Exec)
	}
	if info.Insecure {
		why += ". The kubeconfig turns TLS verification OFF for this cluster"
	}
	g := tools.FirstUse{
		Scope:  tools.FirstUseScope(machinegrant.KindK8sContext, info.Name),
		What:   "kubeconfig context " + info.Name + " at " + displayURL(info.Server),
		Reason: why, Kind: machinegrant.KindK8sContext, Name: info.Name, Identity: info.Identity,
	}
	if !m.cfg.KeptLogins {
		// Not the person's own machine (a server's operator kubeconfig), or a
		// guest: asked in each session, and nothing is kept.
		g.Scope = ""
		if m.cfg.SessionLogins {
			g.Reason += "; as a guest, approving holds for this session only"
		}
	}
	return g, true
}

// keepFirstUse keeps g for the person.
func keepFirstUse(g tools.FirstUse) error {
	return machinegrant.Keep(machinegrant.Grant{Kind: g.Kind, Name: g.Name, Identity: g.Identity, What: g.What})
}

// boundSecrets are the stored secrets a call reads through a kept login it
// is not yet logged in to in this session.
func (m *Manager) boundSecrets(sess *tools.Session, raw json.RawMessage) []string {
	var a struct {
		Cluster string `json:"cluster"`
	}
	if m == nil || json.Unmarshal(raw, &a) != nil || a.Cluster == "" {
		return nil
	}
	if l := m.logins(sess, false); l != nil && l.has(a.Cluster) {
		return nil
	}
	if kl, ok := m.keptLogin(a.Cluster); ok {
		return []string{kl.Secret}
	}
	return nil
}

// loginKept logs the session in to a kept login with its bound secret.
func (m *Manager) loginKept(sess *tools.Session, name string) error {
	kl, ok := m.keptLogin(name)
	if !ok {
		return fmt.Errorf("this session has not logged in to cluster %q; call k8s_login first", name)
	}
	if m.secret == nil {
		return fmt.Errorf("no secrets store is available here, so the token kept for cluster %s cannot be read", name)
	}
	token, err := m.secret(kl.Secret)
	if err != nil {
		return fmt.Errorf("cluster %s: the token kept as secret %s cannot be read: %v", name, kl.Secret, err)
	}
	return m.login(sess, sessionCred{token: strings.TrimSpace(token), cluster: keptCluster(name, kl), namespace: kl.Namespace})
}

// FirstUse makes a call's first use of a kubeconfig context ask.
func (t GetTool) FirstUse(sess *tools.Session, raw json.RawMessage) (tools.FirstUse, bool) {
	return t.M.firstUse(sess, raw)
}

// KeepFirstUse keeps a context's grant for the person.
func (GetTool) KeepFirstUse(g tools.FirstUse) error { return keepFirstUse(g) }

// BoundSecrets names the secret a kept login reads.
func (t GetTool) BoundSecrets(sess *tools.Session, raw json.RawMessage) []string {
	return t.M.boundSecrets(sess, raw)
}

// FirstUse makes a write's first use of a kubeconfig context say so; the
// write asks each time regardless.
func (t ApplyTool) FirstUse(sess *tools.Session, raw json.RawMessage) (tools.FirstUse, bool) {
	return t.M.firstUse(sess, raw)
}

// KeepFirstUse keeps a context's grant for the person.
func (ApplyTool) KeepFirstUse(g tools.FirstUse) error { return keepFirstUse(g) }

// BoundSecrets names the secret a kept login reads.
func (t ApplyTool) BoundSecrets(sess *tools.Session, raw json.RawMessage) []string {
	return t.M.boundSecrets(sess, raw)
}

// hidden reports that the reads and writes are not offered yet: the manager
// stands for k8s_login alone until a login exists.
func (m *Manager) hidden() bool {
	if m == nil || !m.cfg.HiddenUntilLogin || m.loggedIn.Load() {
		return false
	}
	return len(m.keptNames()) == 0
}

// Hidden keeps k8s_get from the model until there is a cluster to read.
func (t GetTool) Hidden() bool { return t.M.hidden() }

// Hidden keeps k8s_apply from the model until there is a cluster to change.
func (t ApplyTool) Hidden() bool { return t.M.hidden() }
