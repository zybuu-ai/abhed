package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/docsite"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
)

// serveCmd starts server mode: web console, REST API, and SSE streaming over
// the same event stream the CLI consumes.
func (a *App) serveCmd(workspace, addr string) int {
	cfg, err := a.loadConfig(workspace)
	if err == nil {
		// Serving without the file's auth or storage would fail open.
		err = cfg.Workspace.DeploymentError("serve")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		var ed *EditionError
		if errors.As(err, &ed) {
			return 2
		}
		return 1
	}
	provider, err := cfg.Provider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if err := vaultLoads(); err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}

	sb, err := buildSandbox(cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	// Under the fence each session gets a fence of its own, with its own
	// cgroup, qualified at its first command; this one qualifies the host at
	// startup and confines nothing else.
	fence := fenceOf(sb)
	if fence != nil {
		defer func() { _ = fence.Close() }()
		if fence.Mode() != sandbox.FenceModeMounts {
			fmt.Fprintln(os.Stderr, "abhed: sandbox.tier is fence, and serve needs its mount_namespace mode, which this host does not give an ordinary user; "+
				"serve refuses rather than run commands under another tier. Unset sandbox.tier to serve, or run it where user namespaces are allowed.")
			return 1
		}
	}
	vault := openVault()
	bash := tools.Bash{Sandbox: sb.Command,
		Isolation: sandboxconfig.Isolation(cfg, string(sb.Tier()))}
	// The workbench terminal's shell runs under the same backend as the agent's commands.
	if in, ok := sb.(sandbox.Interactive); ok {
		bash.Shell, bash.Isolation.Backend = in.Shell, in.Backend()
	}
	// Terminal containers a crashed run of this server left behind.
	if sw, ok := sb.(interface{ SweepShells() }); ok {
		go sw.SweepShells()
	}
	// The CLI's tool set. The server shares its registry across sessions and
	// binds each session's own subagents, todo list and skill tool to it.
	set := toolset.Build(context.Background(), cfg, toolset.Options{
		Workspace: workspace, Bash: bash, Parts: toolset.All, Vault: vault, Warn: warnf,
	})
	defer set.Close()

	// Identity first: a provider that cannot reach its issuer is a startup
	// finding, and the rest of the banner describes a server that will not
	// start.
	authMW, err := a.buildAuth(context.Background(), cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	fmt.Printf("auth        %s\n", authLabel(cfg, authMW))
	fmt.Printf("web search  %s\n", webSearchLabel(cfg))
	fmt.Printf("web fetch   %s\n", webFetchLabel(cfg))
	if line, failed := extensionsLabel(cfg, set); line != "" {
		fmt.Printf("extensions  %s\n", line)
		// Every session runs without a veto that did not start, so it is said where the operator looks.
		for _, name := range failed {
			fmt.Fprintf(os.Stderr, "abhed: warning: extension %s is not running; sessions run without its veto\n", name)
		}
	}
	if cfg.K8s.Enabled {
		writes := "read-only"
		if cfg.K8s.AllowWrites {
			writes = "reads + writes (every write needs approval)"
		}
		fmt.Printf("kubernetes  %s\n", writes)
		// Resolve the kubeconfig now: a misconfigured cluster should be a
		// startup finding, not a surprise mid-task.
		if c, err := k8s.Open(k8s.Config{Kubeconfig: cfg.K8s.Kubeconfig,
			Context: cfg.K8s.Context, Namespace: cfg.K8s.Namespace,
			Token: os.Getenv("ABHED_K8S_TOKEN")}); err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
		} else {
			fmt.Printf("            context %s · namespace %s\n", c.Name, c.Namespace)
		}
		for _, lc := range toolset.LoginClusters(cfg) {
			label := lc.Name + " → " + lc.Server
			if lc.InsecureSkipTLSVerify {
				label += " ⚠ no TLS verification"
			}
			fmt.Printf("k8s login   %s\n", label)
		}
	}
	// Note the absence of a len(Hosts) > 0 condition. A deployment that enables
	// SSH with no hosts listed still gets the tools, because ssh_connect is how
	// a host gets declared in the first place — requiring one in config to use
	// the tool that adds them was the bug.
	if cfg.SSH.Enabled {
		names := make([]string, 0, len(cfg.SSH.Hosts))
		for _, h := range cfg.SSH.Hosts {
			label := h.Name
			if h.InsecureSkipHostKeyCheck {
				label += " ⚠ no host key check"
			}
			names = append(names, label)
		}
		fmt.Printf("ssh hosts   %s\n", strings.Join(names, ", "))
	}
	if n := len(cfg.RAG.Corpora); n > 0 {
		names := make([]string, 0, n)
		for _, c := range cfg.RAG.Corpora {
			if c.Enabled {
				names = append(names, c.Name)
			}
		}
		if len(names) > 0 {
			fmt.Printf("rag corpora %s\n", strings.Join(names, ", "))
		}
	}
	fmt.Printf("storage     %s\n", serveStorageLabel(cfg))
	if cfg.Storage.Driver == "postgres" {
		st, closeFn, err := openStore(context.Background(), cfg)
		if err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
		} else {
			if pg, ok := st.(*store.Postgres); ok {
				printStoreStatus(pg)
			}
			closeFn()
		}
	}
	eventStore, closeStore, err := openServeStore(context.Background(), cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	defer closeStore()

	// Taps on the event store: an exporter being slow or absent costs spans,
	// never turns. Several are fanned in; each is unaware of the others.
	var taps []func(agent.Event)
	for _, build := range a.taps {
		tap, stop, err := build(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		if stop != nil {
			defer stop()
		}
		if tap != nil {
			taps = append(taps, tap)
		}
	}

	opts := server.Options{
		Addr:         addr,
		Workspace:    workspace,
		HomeURL:      cfg.Server.HomeURL,
		EventTap:     fanIn(taps),
		Config:       cfg,
		Adapter:      buildAdapter(provider),
		Registry:     set.Registry,
		Redact:       openVault().Live(),
		SkillListing: set.SkillListing,
		SkillDirs:    set.SkillDirs(),
		Extensions:   set.Extensions,
		Store:        eventStore,
		Auth:         authMW,
		// The live objects behind the settings surface. Passing the registries
		// rather than only their rendered output is what lets a change reach
		// the next session without a restart.
		// Kept rather than discarded: the settings surface can trigger a
		// reindex, which needs the same Index the search tool is reading.
		SkillRegistry: set.Skills,
		SkillRoots:    toolset.SkillRoots(cfg),
		Agents:        set.Agents,
		Gateway:       set.Gateway,
		Index:         set.Index,
		IndexOptions:  toolset.IndexOptions(cfg),
		DrainTimeout:  time.Duration(cfg.Server.DrainSeconds) * time.Second,
	}
	// A session leaving this process takes its egress proxy with it.
	if es, ok := sb.(interface{ EndSession(string) error }); ok {
		opts.SessionEnded = func(id string) { _ = es.EndSession(id) }
	}
	if fence != nil {
		opts.SessionBash = fenceSessionBash(cfg, workspace)
	}
	for _, h := range a.serverOpts {
		if err := h(cfg, &opts); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 2
		}
	}
	srv := server.New(opts)
	// Every session's egress proxy stops with the server, before the store
	// closes, so the last summaries it records are kept.
	if c, ok := sb.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}

	stopper := cancelOnStop(stopDrains)
	defer stopper.stop()
	ctx := stopper.ctx

	for _, h := range a.serveHooks {
		stopHook, err := h(ctx, srv, cfg)
		if err != nil {
			// A hook failing is a startup refusal, like a bad config: nothing
			// has been served yet and the operator is looking at the terminal.
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 2
		}
		if stopHook != nil {
			defer stopHook()
		}
	}

	bs := ui.NewStyle(os.Stdout)
	fmt.Printf("%s %s %s  %s\n", bs.Accent(ui.Glyph),
		bs.Bold("ABHED"), bs.Dim(a.version), browsableURL(addr))
	fmt.Printf("  workspace %s\n  model     %s\n  sandbox   %s\n  storage   %s\n",
		workspace, provider.Model, sb.Tier(), serveStorageLabel(cfg))
	fmt.Printf("  auth      %s\n", authLabel(cfg, authMW))
	if line := managedLine(cfg); line != "" {
		fmt.Printf("  managed   %s\n", line)
	}
	switch {
	case docsite.Available():
		fmt.Printf("  docs      %s/docs\n", browsableURL(addr))
	case docsite.PublicURL != "":
		// Not embedded in this build; say where the same pages are, once,
		// here — never from a page, which must stay usable with no route out.
		fmt.Printf("  docs      %s (not embedded in this build)\n", docsite.PublicURL)
	}

	if err := srv.ListenAndServe(ctx); err != nil && err.Error() != "http: Server closed" {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	return 0
}

// fanIn joins taps into one, or none, so the server never wraps its store
// around a function that does nothing.
func fanIn(taps []func(agent.Event)) func(agent.Event) {
	switch len(taps) {
	case 0:
		return nil
	case 1:
		return taps[0]
	}
	return func(ev agent.Event) {
		for _, t := range taps {
			t(ev)
		}
	}
}

// browsableURL turns a listen address into one that can be pasted into a
// browser. The banner used to print "http://localhost" + addr, which is right
// for the default ":8080" and nonsense for anything else: binding
// "127.0.0.1:8080" (as the container does) announced
// "http://localhost127.0.0.1:8080".
//
// A wildcard bind has no single correct URL, so it resolves to localhost, which
// is the one address the person reading the banner is certainly able to reach.
func browsableURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// buildAuth constructs the identity layer from the registered builders.
//
// Local accounts and an identity provider are independent capabilities, not
// alternatives. A team may well want both — passwords for contractors who are
// not in the corporate directory, an identity provider for staff — so "local"
// enables the password path and a configured provider adds the button beside
// it. Proxy mode is neither: it trusts headers, which is only safe behind a
// trusted proxy, and there is nothing to build for it.
func (a *App) buildAuth(ctx context.Context, cfg config.Config, workspace string) (*auth.Middleware, error) {
	// Only what must answer before a caller is signed in. Anything else added
	// here is an unauthenticated endpoint on a public port, so the list is
	// kept short deliberately; each provider adds the paths it owns.
	public := server.PublicPaths()
	mw := &auth.Middleware{}

	var users auth.UserStore
	for _, mode := range authModes(cfg) {
		if mode == "proxy" {
			mw.TrustHeaders = true
			continue
		}
		build := a.auth[mode]
		if build == nil {
			return nil, &EditionError{Key: fmt.Sprintf("auth.mode %q", mode), Feature: mode, Edition: a.edition}
		}
		if mode == "local" {
			// Users live in the same Postgres as the event store when one is
			// configured, so accounts survive a restart.
			var err error
			if users, err = userStore(cfg, workspace); err != nil {
				return nil, err
			}
		}
		p, v, err := build(ctx, cfg, workspace, users)
		if err != nil {
			return nil, err
		}
		if p != nil {
			mw.Providers = append(mw.Providers, p)
			public = append(public, p.PublicPaths()...)
		}
		if v != nil {
			mw.Verifier = v
		}
	}
	mw.PublicPaths = public
	return mw, nil
}

// authModes lists the mechanisms a config asks for, in the order they are
// consulted. A local deployment that also names a provider gets both; without
// a client id there is nothing to redirect to, so it stays local-only.
func authModes(cfg config.Config) []string {
	switch cfg.Auth.Mode {
	case "local":
		if cfg.Auth.Provider != "" && cfg.Auth.ClientID != "" {
			return []string{"local", "oidc"}
		}
		return []string{"local"}
	case "oidc", "proxy":
		return []string{cfg.Auth.Mode}
	}
	return nil
}

// authLabel describes the identity layer for the banner and for doctor.
func authLabel(cfg config.Config, mw *auth.Middleware) string {
	var providers []string
	if mw != nil {
		for _, p := range mw.Providers {
			if _, label := p.SignIn(); label != "" {
				providers = append(providers, label)
			}
		}
	}
	switch cfg.Auth.Mode {
	case "local":
		s := "local accounts (username and password)"
		if len(providers) > 0 {
			s += " + " + strings.Join(providers, " + ")
		}
		return s
	case "oidc":
		if len(providers) == 0 {
			return "oidc (bearer tokens; no browser sign-in configured)"
		}
		return "oidc (" + strings.Join(providers, ", ") + ")"
	case "proxy":
		return "proxy headers (only safe behind a trusted proxy)"
	default:
		return "none (single-tenant, no authentication)"
	}
}
