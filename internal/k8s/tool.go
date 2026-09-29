package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Tool gives the agent access to a Kubernetes cluster.
//
// Two things shape the design.
//
// First, reads and writes are different tools, not one tool with a verb
// argument. Abhed's permission engine decides by tool name and arguments, and
// a single k8s tool would force it to parse an opaque verb to tell "list pods"
// from "delete namespace". Splitting them means the read tool is genuinely
// non-mutating and never prompts, while every write goes through approval by
// construction rather than by correctly interpreting a string.
//
// Second, the cluster is chosen by naming a context the operator already has
// in their kubeconfig. The model cannot supply a server URL or a token, so the
// worst it can do is act on a cluster the person running Abhed can already
// reach — which is the same blast radius as their own kubectl.

// Manager holds the operator's connections, opened lazily and reused. What
// k8s_login adds is kept per session, never here: a server runs every user's
// sessions in one process.
type Manager struct {
	cfg Config

	mu       sync.Mutex
	clusters map[string]*Cluster
}

func NewManager(cfg Config) *Manager {
	return &Manager{cfg: cfg, clusters: map[string]*Cluster{}}
}

// logins is what k8s_login added to one session: credentials keyed by the
// declared cluster's name, and the clients built with them. Memory only.
type logins struct {
	mu       sync.Mutex
	creds    map[string]sessionCred
	clusters map[string]*Cluster
	closed   bool
}

type sessionCred struct {
	token     string
	cluster   LoginCluster
	namespace string
}

// Close forgets the session's credentials and drops its connections, when the
// session goes.
func (l *logins) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	closeClients(l.clusters)
	l.clusters, l.creds, l.closed = map[string]*Cluster{}, map[string]sessionCred{}, true
	return nil
}

func closeClients(cs map[string]*Cluster) {
	for _, c := range cs {
		if c.client != nil {
			c.client.CloseIdleConnections()
		}
	}
}

// loginsKey keys a session's logins by manager, so two managers never meet.
type loginsKey struct{ m *Manager }

// logins returns the session's logins, made when create is set. Nil when the
// session has none, or there is no session to keep them in.
func (m *Manager) logins(sess *tools.Session, create bool) *logins {
	var mk func() any
	if create {
		mk = func() any { return &logins{creds: map[string]sessionCred{}, clusters: map[string]*Cluster{}} }
	}
	l, _ := sess.Scoped(loginsKey{m}, mk).(*logins)
	return l
}

// login records a credential for this session only.
func (m *Manager) login(sess *tools.Session, cred sessionCred) error {
	l := m.logins(sess, true)
	if l == nil {
		return fmt.Errorf("no session to hold the login in")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("the session has ended; nothing was stored")
	}
	l.creds[cred.cluster.Name] = cred
	// Drop the clients built with the old credential, connections included,
	// so the next call uses the new one.
	if c, ok := l.clusters[cred.cluster.Name]; ok {
		closeClients(map[string]*Cluster{"": c})
		delete(l.clusters, cred.cluster.Name)
	}
	return nil
}

// cluster picks the client for a call. A declared cluster the session logged
// in to is reached only with its own login and TLS settings; a kubeconfig
// context only with the operator's credential. A login token is never put on
// a kubeconfig client, whose TLS and exec credential are not what was approved.
func (m *Manager) cluster(sess *tools.Session, clusterName, ctxName string) (*Cluster, error) {
	if clusterName != "" && ctxName != "" {
		return nil, fmt.Errorf("name a cluster you logged in to or a kubeconfig context, not both")
	}
	l := m.logins(sess, false)
	if clusterName != "" {
		if l == nil {
			return nil, fmt.Errorf("this session has not logged in to cluster %q; call k8s_login first", clusterName)
		}
		return l.cluster(m.cfg, clusterName)
	}
	if ctxName == "" && l != nil {
		// With no context named, a single login is the default, as it was
		// the reason for logging in; several need one named.
		if name, n := l.only(); n == 1 {
			return l.cluster(m.cfg, name)
		} else if n > 1 {
			return nil, fmt.Errorf("this session is logged in to more than one cluster; name one "+
				"as cluster (%s), or a kubeconfig context", strings.Join(l.names(), ", "))
		}
	}

	return m.kubeClient(ctxName)
}

// kubeClient returns the operator's client for a kubeconfig context, opened
// once and kept, so an approval and the call it approves name one server.
func (m *Manager) kubeClient(ctxName string) (*Cluster, error) {
	cfg := m.cfg
	if ctxName != "" {
		cfg.Context = ctxName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clusters[ctxName]; ok {
		return c, nil
	}
	c, err := Open(cfg)
	if err != nil {
		return nil, err
	}
	m.clusters[ctxName] = c
	return c, nil
}

// where describes the client cluster() would pick, without opening one.
func (m *Manager) where(sess *tools.Session, clusterName, ctxName string) string {
	if clusterName != "" && ctxName != "" {
		return "names both a cluster and a context, so the call will be refused"
	}
	l := m.logins(sess, false)
	if clusterName == "" && ctxName == "" && l != nil {
		if name, n := l.only(); n == 1 {
			clusterName = name
		} else if n > 1 {
			return "names no cluster while this session is logged in to several, so the call will be refused"
		}
	}
	if clusterName != "" && ctxName == "" {
		for _, lc := range m.cfg.Clusters {
			if lc.Name != clusterName {
				continue
			}
			how := "with this session's login"
			if l == nil || !l.has(clusterName) {
				how = "but this session has not logged in to it, so the call will fail"
			}
			return fmt.Sprintf("changes cluster %s at %s %s, %s", lc.Name, displayURL(lc.Server), how,
				lc.Verification(m.cfg.CAFile))
		}
		return ""
	}
	// The client the call will use, not a fresh read of the file, which may
	// have changed since that client was opened.
	c, err := m.kubeClient(ctxName)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("changes kubeconfig context %s at %s with the kubeconfig's own credential",
		c.Name, displayURL(c.Server))
}

// displayURL is a server URL fit to show and record: any user or password
// written into it is left out.
func displayURL(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return "(a server address that is not a URL)"
	}
	u.User = nil
	return u.String()
}

func (l *logins) has(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.creds[name]
	return ok
}

func (l *logins) only() (string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name := range l.creds {
		return name, len(l.creds)
	}
	return "", 0
}

func (l *logins) names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.creds))
	for name := range l.creds {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// cluster opens, or reuses, the client for one of the session's logins.
func (l *logins) cluster(cfg Config, name string) (*Cluster, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cred, ok := l.creds[name]
	if !ok {
		return nil, fmt.Errorf("this session has not logged in to cluster %q; call k8s_login first", name)
	}
	if c, ok := l.clusters[name]; ok {
		return c, nil
	}
	c, err := OpenLogin(cred.cluster, cfg.CAFile, cred.token, orDefaultNS(cred.namespace, cfg.Namespace))
	if err != nil {
		return nil, err
	}
	l.clusters[name] = c
	return c, nil
}

// ---------------------------------------------------------------- read tool

type GetTool struct{ M *Manager }

func (GetTool) Name() string  { return "k8s_get" }
func (GetTool) Mutates() bool { return false }

func (GetTool) Description() string {
	return "Read from a Kubernetes cluster: list or describe resources, and fetch pod logs. " +
		"Read-only — it cannot create, change or delete anything. " +
		"Use resource plural names as kubectl does (pods, deployments, services, nodes, events)."
}

func (GetTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "resource":{"type":"string","description":"Resource type, plural: pods, deployments, services, nodes, namespaces, events, configmaps. Use 'logs' to fetch pod logs."},
    "name":{"type":"string","description":"A single resource name. Omit to list all of that type."},
    "namespace":{"type":"string","description":"Namespace. Omit for the context's default; use '*' for all namespaces."},
    "cluster":{"type":"string","description":"A declared cluster this session logged in to with k8s_login. Omit to use the only login, or the kubeconfig."},
    "context":{"type":"string","description":"Kubeconfig context naming the cluster. Omit for the current context."},
    "selector":{"type":"string","description":"Label selector, e.g. app=web."},
    "container":{"type":"string","description":"For logs: which container in the pod."},
    "tail":{"type":"integer","description":"For logs: how many trailing lines. Default 200."}
  },
  "required":["resource"]
}`)
}

type getArgs struct {
	Cluster   string `json:"cluster"`
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Context   string `json:"context"`
	Selector  string `json:"selector"`
	Container string `json:"container"`
	Tail      int    `json:"tail"`
}

func (t GetTool) Run(ctx context.Context, sess *tools.Session, raw json.RawMessage) tools.Result {
	var a getArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for k8s_get: %v", err)
	}
	if strings.TrimSpace(a.Resource) == "" {
		return errf("resource is required (pods, deployments, nodes, logs, …)")
	}
	c, err := t.M.cluster(sess, a.Cluster, a.Context)
	if err != nil {
		return errf("%v", err)
	}

	if a.Resource == "logs" {
		return t.logs(ctx, c, a)
	}

	path, err := resourcePath(c, a.Resource, a.Namespace, a.Name)
	if err != nil {
		return errf("%v", err)
	}
	if a.Selector != "" {
		path += "?labelSelector=" + urlEscape(a.Selector)
	}

	data, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: render(a.Resource, data)}
}

func (t GetTool) logs(ctx context.Context, c *Cluster, a getArgs) tools.Result {
	if a.Name == "" {
		return errf("logs needs the pod name in `name`.")
	}
	ns := a.Namespace
	if ns == "" || ns == "*" {
		ns = c.Namespace
	}
	tail := a.Tail
	if tail <= 0 {
		tail = 200
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?tailLines=%d",
		ns, a.Name, tail)
	if a.Container != "" {
		path += "&container=" + urlEscape(a.Container)
	}
	data, err := c.Do(ctx, "GET", path, nil)
	if err != nil {
		return errf("%v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return tools.Result{Content: fmt.Sprintf("Pod %s/%s produced no log output.", ns, a.Name)}
	}
	return tools.Result{Content: string(data)}
}

// ---------------------------------------------------------------- write tool

type ApplyTool struct{ M *Manager }

func (ApplyTool) Name() string { return "k8s_apply" }

// Mutates is true, which is what routes every call through approval. This is
// the whole safety story for cluster writes: there is no mode in which it is
// auto-approved, because a mistaken delete in a production namespace is not
// recoverable by /undo the way a file edit is.
func (ApplyTool) Mutates() bool { return true }

func (ApplyTool) Description() string {
	return "Change a Kubernetes cluster: apply a manifest, scale a workload, delete a resource, " +
		"or restart a rollout. Every call requires human approval. " +
		"Prefer k8s_get first to confirm what you are about to change."
}

func (ApplyTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["apply","delete","scale","restart"],"description":"What to do."},
    "manifest":{"type":"string","description":"For apply: the resource as JSON or YAML-free JSON."},
    "resource":{"type":"string","description":"For delete/scale/restart: resource type, plural."},
    "name":{"type":"string","description":"For delete/scale/restart: the resource name."},
    "namespace":{"type":"string","description":"Namespace. Omit for the context's default."},
    "cluster":{"type":"string","description":"A declared cluster this session logged in to with k8s_login."},
    "context":{"type":"string","description":"Kubeconfig context naming the cluster."},
    "replicas":{"type":"integer","description":"For scale: the desired replica count."}
  },
  "required":["action"]
}`)
}

type applyArgs struct {
	Cluster   string `json:"cluster"`
	Action    string `json:"action"`
	Manifest  string `json:"manifest"`
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Context   string `json:"context"`
	Replicas  *int   `json:"replicas"`
}

// Target tells the person approving a write which cluster it changes, at
// which server, and with whose credential: this session's login or the
// operator's kubeconfig.
func (t ApplyTool) Target(sess *tools.Session, raw json.RawMessage) string {
	var a applyArgs
	if json.Unmarshal(raw, &a) != nil || t.M == nil {
		return ""
	}
	return t.M.where(sess, a.Cluster, a.Context)
}

func (t ApplyTool) Run(ctx context.Context, sess *tools.Session, raw json.RawMessage) tools.Result {
	var a applyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for k8s_apply: %v", err)
	}
	c, err := t.M.cluster(sess, a.Cluster, a.Context)
	if err != nil {
		return errf("%v", err)
	}

	switch a.Action {
	case "apply":
		return t.apply(ctx, c, a)
	case "delete":
		return t.delete(ctx, c, a)
	case "scale":
		return t.scale(ctx, c, a)
	case "restart":
		return t.restart(ctx, c, a)
	}
	return errf("unknown action %q (want apply, delete, scale or restart)", a.Action)
}

func (t ApplyTool) apply(ctx context.Context, c *Cluster, a applyArgs) tools.Result {
	if strings.TrimSpace(a.Manifest) == "" {
		return errf("apply needs a manifest.")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(a.Manifest), &obj); err != nil {
		return errf("manifest must be JSON: %v. Convert YAML to JSON first.", err)
	}
	kind, _ := obj["kind"].(string)
	apiVersion, _ := obj["apiVersion"].(string)
	if kind == "" || apiVersion == "" {
		return errf("manifest needs both apiVersion and kind.")
	}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == "" {
		return errf("manifest metadata.name is required.")
	}
	ns := a.Namespace
	if ns == "" {
		if v, ok := meta["namespace"].(string); ok {
			ns = v
		} else {
			ns = c.Namespace
		}
	}

	base := apiBase(apiVersion) + "/" + pluralFor(kind)
	if isNamespaced(kind) {
		base = apiBase(apiVersion) + "/namespaces/" + ns + "/" + pluralFor(kind)
	}

	// Server-side apply: one PATCH that creates or updates, so there is no
	// read-modify-write race between checking existence and writing.
	path := base + "/" + name + "?fieldManager=abhed&force=true"
	body, _ := json.Marshal(obj)
	data, err := c.doPatch(ctx, path, body, "application/apply-patch+yaml")
	if err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: fmt.Sprintf("Applied %s/%s in %s.\n%s",
		kind, name, ns, summarize(data))}
}

func (t ApplyTool) delete(ctx context.Context, c *Cluster, a applyArgs) tools.Result {
	if a.Resource == "" || a.Name == "" {
		return errf("delete needs resource and name.")
	}
	path, err := resourcePath(c, a.Resource, a.Namespace, a.Name)
	if err != nil {
		return errf("%v", err)
	}
	if _, err := c.Do(ctx, "DELETE", path, nil); err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: fmt.Sprintf("Deleted %s/%s.", a.Resource, a.Name)}
}

func (t ApplyTool) scale(ctx context.Context, c *Cluster, a applyArgs) tools.Result {
	if a.Resource == "" || a.Name == "" || a.Replicas == nil {
		return errf("scale needs resource, name and replicas.")
	}
	path, err := resourcePath(c, a.Resource, a.Namespace, a.Name)
	if err != nil {
		return errf("%v", err)
	}
	body := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, *a.Replicas))
	if _, err := c.doPatch(ctx, path+"/scale", body, "application/merge-patch+json"); err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: fmt.Sprintf("Scaled %s/%s to %d replicas.",
		a.Resource, a.Name, *a.Replicas)}
}

func (t ApplyTool) restart(ctx context.Context, c *Cluster, a applyArgs) tools.Result {
	if a.Resource == "" || a.Name == "" {
		return errf("restart needs resource and name.")
	}
	path, err := resourcePath(c, a.Resource, a.Namespace, a.Name)
	if err != nil {
		return errf("%v", err)
	}
	// The same annotation kubectl rollout restart sets.
	body := []byte(`{"spec":{"template":{"metadata":{"annotations":` +
		`{"abhed.restartedAt":"` + nowRFC3339() + `"}}}}}`)
	if _, err := c.doPatch(ctx, path, body, "application/strategic-merge-patch+json"); err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: fmt.Sprintf("Restarted %s/%s.", a.Resource, a.Name)}
}

// ---------------------------------------------------------------- rendering

// render turns an API list into a compact table. Returning raw JSON would
// spend thousands of tokens on managedFields and resourceVersion, which no
// question ever needs.
func render(resource string, data []byte) string {
	var list struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil || list.Items == nil {
		// A single object: strip the noisiest fields and return it.
		return summarize(data)
	}
	if len(list.Items) == 0 {
		return "No " + resource + " found."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d %s:\n", len(list.Items), resource)
	for _, item := range list.Items {
		meta, _ := item["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		line := name
		if ns != "" {
			line = ns + "/" + name
		}
		if s := statusOf(item); s != "" {
			line += "  " + s
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// statusOf extracts the one or two facts that matter per resource kind.
func statusOf(item map[string]any) string {
	status, _ := item["status"].(map[string]any)
	if status == nil {
		return ""
	}
	var parts []string
	if phase, ok := status["phase"].(string); ok {
		parts = append(parts, phase)
	}
	// Pods: ready containers and restarts, which is what a person looks at.
	if cs, ok := status["containerStatuses"].([]any); ok {
		ready, restarts := 0, 0
		for _, e := range cs {
			c, _ := e.(map[string]any)
			if r, _ := c["ready"].(bool); r {
				ready++
			}
			if n, ok := c["restartCount"].(float64); ok {
				restarts += int(n)
			}
		}
		parts = append(parts, fmt.Sprintf("%d/%d ready", ready, len(cs)))
		if restarts > 0 {
			parts = append(parts, strconv.Itoa(restarts)+" restarts")
		}
	}
	// Deployments.
	if r, ok := status["readyReplicas"].(float64); ok {
		if total, ok := status["replicas"].(float64); ok {
			parts = append(parts, fmt.Sprintf("%d/%d replicas", int(r), int(total)))
		}
	}
	return strings.Join(parts, "  ")
}

// summarize removes the fields that dominate a manifest's size and carry no
// information a question depends on.
func summarize(data []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return string(data)
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		for _, noise := range []string{"managedFields", "resourceVersion", "uid",
			"generation", "selfLink", "creationTimestamp"} {
			delete(meta, noise)
		}
	}
	out, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return string(data)
	}
	if len(out) > 24000 {
		return string(out[:24000]) + "\n…[truncated; ask for a specific field]"
	}
	return string(out)
}

// ---------------------------------------------------------------- paths

var coreResources = map[string]bool{
	"pods": true, "services": true, "nodes": true, "namespaces": true,
	"events": true, "configmaps": true, "secrets": true,
	"persistentvolumes": true, "persistentvolumeclaims": true,
	"serviceaccounts": true, "endpoints": true, "replicationcontrollers": true,
}

var appsResources = map[string]bool{
	"deployments": true, "statefulsets": true, "daemonsets": true, "replicasets": true,
}

var clusterScoped = map[string]bool{
	"nodes": true, "namespaces": true, "persistentvolumes": true,
	"clusterroles": true, "clusterrolebindings": true, "storageclasses": true,
}

func resourcePath(c *Cluster, resource, namespace, name string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(resource))
	r = normalizeResource(r)

	var base string
	switch {
	case coreResources[r]:
		base = "/api/v1"
	case appsResources[r]:
		base = "/apis/apps/v1"
	case r == "jobs":
		base = "/apis/batch/v1"
	case r == "cronjobs":
		base = "/apis/batch/v1"
	case r == "ingresses":
		base = "/apis/networking.k8s.io/v1"
	default:
		return "", fmt.Errorf("resource %q is not one Abhed knows how to address. "+
			"Supported: %s. For anything else, use bash with kubectl",
			resource, strings.Join(knownResources(), ", "))
	}

	if clusterScoped[r] {
		if name != "" {
			return base + "/" + r + "/" + name, nil
		}
		return base + "/" + r, nil
	}

	ns := namespace
	if ns == "" {
		ns = c.Namespace
	}
	if ns == "*" {
		if name != "" {
			return "", fmt.Errorf("naming a single %s needs a namespace, not '*'", r)
		}
		return base + "/" + r, nil
	}
	if name != "" {
		return base + "/namespaces/" + ns + "/" + r + "/" + name, nil
	}
	return base + "/namespaces/" + ns + "/" + r, nil
}

// normalizeResource accepts the singular and short forms people type.
func normalizeResource(r string) string {
	switch r {
	case "po", "pod":
		return "pods"
	case "deploy", "deployment":
		return "deployments"
	case "svc", "service":
		return "services"
	case "ns", "namespace":
		return "namespaces"
	case "no", "node":
		return "nodes"
	case "cm", "configmap":
		return "configmaps"
	case "sts", "statefulset":
		return "statefulsets"
	case "ds", "daemonset":
		return "daemonsets"
	case "rs", "replicaset":
		return "replicasets"
	case "ing", "ingress":
		return "ingresses"
	case "job":
		return "jobs"
	case "cj", "cronjob":
		return "cronjobs"
	case "ev", "event":
		return "events"
	case "pvc":
		return "persistentvolumeclaims"
	case "pv":
		return "persistentvolumes"
	case "sa":
		return "serviceaccounts"
	case "secret":
		return "secrets"
	}
	return r
}

func knownResources() []string {
	set := map[string]bool{"jobs": true, "cronjobs": true, "ingresses": true}
	for k := range coreResources {
		set[k] = true
	}
	for k := range appsResources {
		set[k] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func apiBase(apiVersion string) string {
	if apiVersion == "v1" {
		return "/api/v1"
	}
	return "/apis/" + apiVersion
}

func pluralFor(kind string) string {
	k := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(k, "s"):
		return k + "es"
	case strings.HasSuffix(k, "y"):
		return k[:len(k)-1] + "ies"
	}
	return k + "s"
}

func isNamespaced(kind string) bool {
	switch strings.ToLower(kind) {
	case "namespace", "node", "persistentvolume", "clusterrole",
		"clusterrolebinding", "storageclass", "customresourcedefinition":
		return false
	}
	return true
}

func errf(format string, a ...any) tools.Result {
	return tools.Result{Content: fmt.Sprintf(format, a...), IsError: true}
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~', r == '=', r == ',':
			b.WriteRune(r)
		default:
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- login tool

// LoginTool logs in to a cluster during a conversation.
//
// This exists because of a real failure: a user pasted an `oc login --token=...
// --server=...` command into the chat, approved the agent running it, and got
// nothing. Three things had gone wrong at once. The sandbox blocks reads of
// ~/.kube, so `oc` could not authenticate. Even had it worked, each bash call
// is a fresh sandboxed process, so the login would not have survived to the
// next call. And the kubeconfig's own token had expired, so the native tools
// were failing too.
//
// Handling the credential directly fixes all three: it never touches the
// sandbox, it lives with the session, and it replaces the stale kubeconfig
// entry for that session.
//
// The token is taken from the secrets store by name, never as an argument: an
// argument is judged, shown for approval, recorded and sent back to the model
// on every turn. It goes only to a cluster the operator declared in
// k8s.clusters, over verified TLS: a server the model chose could be anyone's.
// The login belongs to the session that made it, since a server runs every
// user's sessions in one process.
type LoginTool struct {
	M *Manager
	// Secret returns a stored secret's value; nil means no store.
	Secret func(name string) (string, error)
	// SecretNames is what the model may name, listed in the description.
	SecretNames []string
}

func (LoginTool) Name() string { return "k8s_login" }

// Mutates is true. Nothing in the cluster changes, but the agent's authority
// does: this is the call that decides which cluster it can reach and as whom.
// That deserves the same confirmation as a write.
func (LoginTool) Mutates() bool { return true }

// FixedArgs: a token sent the old way is dropped, not kept in the record.
func (LoginTool) FixedArgs() {}

// SecretArgs puts token_secret to a secret(NAME) rule.
func (LoginTool) SecretArgs() []string { return []string{"token_secret"} }

func (t LoginTool) Description() string {
	d := "Authenticate to a Kubernetes or OpenShift cluster, for this session only, with a " +
		"token the user has stored with `abhed secret set NAME`. Pass the cluster's name from " +
		"the list below and the secret's NAME, never the token or a server URL. When the user " +
		"offers a token or pastes an `oc login --token=... --server=...` command, ask them to " +
		"store the token with `abhed secret set NAME` and tell you the name. " +
		"Do NOT run `oc login` through bash: the sandbox blocks access to the kubeconfig, " +
		"and a login inside a bash call does not survive to the next one."
	if names := t.clusterNames(); len(names) > 0 {
		d += " Clusters: " + strings.Join(names, ", ") + "."
	} else {
		d += " No clusters are declared for login; the operator adds them in k8s.clusters."
	}
	if len(t.SecretNames) > 0 {
		d += " Stored secrets: " + strings.Join(t.SecretNames, ", ") + "."
	}
	return d
}

func (LoginTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "cluster":{"type":"string","description":"Name of a cluster the operator declared, e.g. prod. Not a URL."},
    "token_secret":{"type":"string","description":"NAME of the stored secret holding the bearer token, e.g. OCP_TOKEN. Never the token."},
    "namespace":{"type":"string","description":"Default namespace for later calls."}
  },
  "required":["cluster","token_secret"]
}`)
}

type loginArgs struct {
	Cluster     string `json:"cluster"`
	TokenSecret string `json:"token_secret"`
	Namespace   string `json:"namespace"`
}

func (t LoginTool) clusterNames() []string {
	if t.M == nil {
		return nil
	}
	out := make([]string, 0, len(t.M.cfg.Clusters))
	for _, c := range t.M.cfg.Clusters {
		out = append(out, c.Name)
	}
	return out
}

// declared finds the named cluster. Anything else, a URL included, is refused
// before a secret is read or a request is made.
func (t LoginTool) declared(name string) (LoginCluster, error) {
	name = strings.TrimSpace(name)
	if t.M != nil {
		for _, c := range t.M.cfg.Clusters {
			if c.Name == name {
				return c, nil
			}
		}
	}
	names := t.clusterNames()
	if len(names) == 0 {
		return LoginCluster{}, fmt.Errorf("no clusters are declared for k8s_login; the operator " +
			"adds them in k8s.clusters, and a token is sent to no other server")
	}
	return LoginCluster{}, fmt.Errorf("%q is not a declared cluster (declared: %s). A token is "+
		"sent only to a cluster the operator declared in k8s.clusters, never to a URL",
		name, strings.Join(names, ", "))
}

// Precheck refuses an undeclared cluster before anyone is asked to approve it.
func (t LoginTool) Precheck(_ *tools.Session, raw json.RawMessage) error {
	var a loginArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	_, err := t.declared(a.Cluster)
	return err
}

// Target tells the person approving, and the record, where the token goes.
func (t LoginTool) Target(_ *tools.Session, raw json.RawMessage) string {
	var a loginArgs
	if json.Unmarshal(raw, &a) != nil {
		return ""
	}
	lc, err := t.declared(a.Cluster)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("sends the token in secret %s to cluster %s at %s, %s",
		a.TokenSecret, lc.Name, displayURL(lc.Server), lc.Verification(t.M.cfg.CAFile))
}

func (t LoginTool) Run(ctx context.Context, sess *tools.Session, raw json.RawMessage) tools.Result {
	var a loginArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for k8s_login: %v", err)
	}
	// Without a session the login would have nowhere of its own to live.
	if sess == nil {
		return errf("k8s_login needs a session to hold the login; nothing was stored.")
	}
	lc, err := t.declared(a.Cluster)
	if err != nil {
		return errf("%v", err)
	}
	a.TokenSecret = strings.TrimSpace(a.TokenSecret)
	if a.TokenSecret == "" {
		return errf("token_secret is required. Ask the user to store the token with " +
			"`abhed secret set NAME` and pass that NAME as token_secret.")
	}
	if !secrets.ValidName(a.TokenSecret) {
		return errf("token_secret is the NAME of a stored secret, such as OCP_TOKEN, not the " +
			"token. Ask the user to store the token with `abhed secret set NAME`.")
	}
	if t.Secret == nil {
		return errf("No secrets store is available here, so k8s_login cannot read a token.")
	}
	token, err := t.Secret(a.TokenSecret)
	if err != nil {
		return errf("%v", err)
	}
	token = strings.TrimSpace(token)

	c, err := OpenLogin(lc, t.M.cfg.CAFile, token, orDefaultNS(a.Namespace, t.M.cfg.Namespace))
	if err != nil {
		return errf("%v", err)
	}
	// Only the check uses this client; the session opens its own.
	defer c.client.CloseIdleConnections()
	// Verify before reporting success. Storing a credential that does not work
	// would turn one clear failure into a confusing one on the next call.
	if _, err := c.Do(ctx, "GET", "/version", nil); err != nil {
		return errf("Could not authenticate to cluster %s at %s: %v", lc.Name, displayURL(lc.Server), err)
	}

	if err := t.M.login(sess, sessionCred{token: token, cluster: lc, namespace: a.Namespace}); err != nil {
		return errf("%v", err)
	}
	return tools.Result{Content: fmt.Sprintf(
		"Authenticated to cluster %s at %s (namespace %s, %s) with secret %s. The login "+
			"holds for this session only and is not written to your kubeconfig. "+
			"Name it as cluster %q in k8s_get and k8s_apply.", lc.Name, displayURL(lc.Server), c.Namespace,
		lc.Verification(t.M.cfg.CAFile), a.TokenSecret, lc.Name)}
}

func orDefaultNS(a, b string) string {
	if a != "" {
		return a
	}
	if b != "" {
		return b
	}
	return "default"
}
