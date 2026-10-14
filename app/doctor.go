package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/driver/builtin"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/agentdefs"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/store"
)

// doctorTimeout bounds the endpoint checks unless -timeout says otherwise.
const doctorTimeout = 60 * time.Second

// doctorArgs reads doctor's own arguments: --json, and -timeout for the
// endpoint checks.
func doctorArgs(args []string) (asJSON bool, timeout time.Duration, err error) {
	timeout = doctorTimeout
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue, _ := splitFlag(arg)
		switch name {
		case "json":
			asJSON = true
		case "timeout":
			if !hasValue {
				if i+1 >= len(args) {
					return false, 0, errors.New("-timeout needs a value, such as 2m")
				}
				i++
				value = args[i]
			}
			if timeout, err = time.ParseDuration(value); err != nil || timeout <= 0 {
				return false, 0, fmt.Errorf("-timeout %s is not a positive duration, such as 2m", config.Printable(value))
			}
		}
	}
	return asJSON, timeout, nil
}

// doctor verifies the endpoint actually works before the user debugs it
// through a failing agent run.
func (a *App) doctor(workspace string, set flagSettings, timeout time.Duration) int {
	cfg, err := a.loadConfigWith(workspace, set)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		var ed *EditionError
		if errors.As(err, &ed) {
			return 2
		}
		return 1
	}
	provider, err := cfg.Provider()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}

	fmt.Printf("%s %s\n\n", ui.NewStyle(os.Stdout).Accent(ui.Glyph),
		ui.NewStyle(os.Stdout).Bold("abhed doctor"))
	fmt.Printf("edition     %s\n", a.edition)
	fmt.Printf("workspace   %s\n", workspace)
	fmt.Printf("config      %s\n", configFilesLine(cfg))
	fmt.Printf("provider    %s (%s)\n", cfg.Model.Default, provider.Type)
	fmt.Printf("endpoint    %s\n", provider.BaseURL)
	fmt.Printf("model       %s\n", provider.Model)
	fmt.Printf("mode        %s\n", modeLabel(cfg))
	if line := managedLine(cfg); line != "" {
		fmt.Printf("managed     %s\n", line)
	}
	if cfg.Settings.Name != "" {
		fmt.Printf("settings    %s\n", config.Printable(cfg.Settings.Name))
	}
	printDoctorTrust(os.Stdout, cfg.Workspace)
	printDoctorStartupFiles(os.Stdout)
	// A managed definition named with another case is silently not read.
	for _, w := range agentdefs.ManagedCaseWarnings(managed.AgentsDir) {
		fmt.Printf("agents      ⚠ %s\n", w)
	}
	findings := configFindings(os.Stdout, cfg)
	var tier sandbox.Tier // the git line's: unknown when no sandbox could be built
	if sb, err := buildSandbox(cfg, workspace); err == nil {
		tier = sb.Tier()
		defer func() {
			if err := sandbox.Close(sb); err != nil {
				fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			}
		}()
		label := tierLabel(sb)
		if sb.Tier() == sandbox.TierNone {
			label += "  ⚠"
		}
		fmt.Printf("sandbox     %s — %s\n", label, sb.Describe())
		for _, w := range limitWarnings(cfg, sb.Tier()) {
			fmt.Printf("            ⚠ %s\n", w)
		}
		// MCP servers' containers a killed Abhed left, removed as a session
		// would; only when one is set to run in a container.
		if c, ok := sb.(*sandbox.Container); ok && confinedStdioServers(cfg) {
			sw := c.SweepServers(context.Background())
			if len(sw.Removed) > 0 {
				fmt.Printf("            removed %d MCP server container(s) an Abhed process left when it was killed: %s\n", len(sw.Removed), strings.Join(sw.Removed, ", "))
			}
			if len(sw.Unchecked) > 0 {
				fmt.Printf("            kept %d MCP server container(s) whose Abhed process cannot be checked from here (another user's?): %s; remove them yourself if they are left over\n", len(sw.Unchecked), strings.Join(sw.Unchecked, ", "))
			}
		}
	} else {
		fmt.Printf("sandbox     UNAVAILABLE — %v\n", err)
	}
	fmt.Printf("egress      %s\n", egressLabel(cfg))
	for _, w := range internalModelEndpoints(cfg) {
		fmt.Printf("            ⚠ %s\n", w)
	}
	if w := pathDenyWarning(cfg); w != "" {
		fmt.Printf("            ⚠ %s\n", w)
	}
	fmt.Printf("git         %s\n", gitHeldLabel(cfg, tier))
	// The sessions line as serve would run them: a sandbox each with several accounts.
	served := cfg
	if c, iso, err := serveSessionIsolation(cfg, workspace); err == nil && iso.auto {
		served = c
	}
	if printDrivers(os.Stdout, served) {
		findings.driverDown = true
	}
	if cfg.Sandbox.Tier == string(sandbox.TierFence) {
		printFenceProbe(os.Stdout, cfg, workspace)
	}
	if files := agent.DiscoverMemoryFiles(workspace); len(files) > 0 {
		fmt.Printf("memory      %s\n", strings.Join(files, ", "))
	}
	if mw, err := a.buildAuth(context.Background(), cfg, workspace); err != nil {
		fmt.Printf("auth        %s\n            UNAVAILABLE — %v\n", authLabel(cfg, nil), err)
		findings.authDown = true
	} else {
		fmt.Printf("auth        %s\n", authLabel(cfg, mw))
		if line := sessionLine(cfg); line != "" {
			fmt.Printf("            %s\n", line)
		}
		// A provider that can prove itself does so here, so a misconfigured
		// issuer is a doctor finding rather than the first user's error page.
		for _, p := range mw.Providers {
			if c, ok := p.(auth.Checker); ok {
				if err := c.Check(context.Background()); err != nil {
					fmt.Printf("            %s UNAVAILABLE — %v\n", p.Name(), err)
				} else {
					fmt.Printf("            %s ready\n", p.Name())
				}
			}
		}
	}
	// The same checks serve makes before it listens.
	if cfg.Server.TLS.Enabled() {
		if l, err := buildTLS(cfg, workspace, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
			fmt.Printf("tls         UNAVAILABLE — %v\n", err)
			findings.tlsDown = true
		} else {
			fmt.Printf("tls         %s\n", tlsLabel(cfg, l))
			for _, w := range l.Warnings() {
				fmt.Printf("            ⚠ %s\n", w)
			}
		}
	}
	fmt.Printf("web search  %s\n", webSearchLabel(cfg))
	fmt.Printf("web fetch   %s\n", webFetchLabel(cfg))
	vaultErr := vaultLoads()
	if vaultErr != nil {
		fmt.Printf("secrets     UNAVAILABLE — %v\n", vaultErr)
	} else if names := toolset.VaultNames(openVault()); len(names) > 0 {
		fmt.Printf("secrets     %d stored in %s\n", len(names), openVault().Path())
	}
	if reg, _ := toolset.LoadSkills(cfg, warnf); reg.Len() > 0 {
		fmt.Printf("skills      %d loaded: %s\n", reg.Len(),
			ui.VisibleLine(strings.Join(reg.Names(), ", ")))
	}
	if cfg.K8s.Enabled {
		writes := "read-only"
		if cfg.K8s.AllowWrites {
			writes = "reads + writes (every write needs approval)"
		}
		fmt.Printf("kubernetes  %s\n", writes)
		if c, err := k8s.Open(k8s.Config{Kubeconfig: cfg.K8s.Kubeconfig,
			Context: cfg.K8s.Context, Namespace: cfg.K8s.Namespace,
			Token: os.Getenv("ABHED_K8S_TOKEN")}); err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
		} else {
			fmt.Printf("            context %s\n            namespace %s · server %s\n",
				c.Name, c.Namespace, c.Server)
			if c.Insecure() {
				fmt.Printf("            ⚠ the kubeconfig skips TLS verification for this cluster\n")
			}
		}
		for _, lc := range toolset.LoginClusters(cfg) {
			warn := ""
			if lc.InsecureSkipTLSVerify {
				warn = "  ⚠ TLS verification disabled"
			}
			fmt.Printf("k8s login   %s → %s%s\n", lc.Name, lc.Server, warn)
		}
	}
	if cfg.SSH.Enabled && len(cfg.SSH.Hosts) > 0 {
		for _, h := range cfg.SSH.Hosts {
			warn := ""
			if h.InsecureSkipHostKeyCheck {
				warn = "  ⚠ host key verification disabled"
			}
			fmt.Printf("ssh host    %s → %s@%s%s\n", h.Name, h.User, h.Addr, warn)
		}
	}
	for _, c := range cfg.RAG.Corpora {
		if c.Enabled {
			fmt.Printf("rag corpus  %s → %s\n", c.Name, c.URL)
		}
	}
	if cfg.Storage.Driver == "postgres" {
		fmt.Printf("storage     %s\n", storageLabel(cfg))
		st, closeFn, err := openStore(context.Background(), cfg)
		if err != nil {
			fmt.Printf("            UNAVAILABLE — %v\n", err)
			findings.storeDown = true
		} else {
			if pg, ok := st.(*store.Postgres); ok {
				printStoreStatus(pg)
			}
			closeFn()
		}
	} else {
		// serve keeps sessions in memory; the command line keeps its own record.
		fmt.Printf("storage     %s for serve\n            %s for the command line\n", serveStorageLabel(cfg), storageLabel(cfg))
	}
	fmt.Printf("sandboxes   %s\n", controlPlaneLabel(cfg))
	if cfg.ControlPlane.Enabled {
		if label, err := sshKeyLabel(cfg); err != nil {
			fmt.Printf("ssh relay   UNAVAILABLE — %v\n", err)
		} else {
			fmt.Printf("ssh relay   host key %s\n", label)
		}
	}
	if cfg.Retrieval.Enabled {
		if ix, err := toolset.OpenIndex(context.Background(), cfg, workspace); err == nil {
			d, t, v, _ := ix.Stats()
			fmt.Printf("index       %d chunks · %d terms · %d vectors\n", d, t, v)
		} else {
			fmt.Printf("index       UNAVAILABLE — %v\n", err)
		}
	}
	if servers := cfg.MCP.Servers; len(servers) > 0 {
		gw := mcp.NewGateway()
		// The servers are started as a session starts them: their files
		// confined, and under the allowlist their network too.
		if own := toolset.OwnEgress(cfg); own != nil {
			defer own.Close()
			defer egress.Install(own)()
			gw.Egress = own
		}
		var unconfined []string
		note := func(format string, a ...any) { unconfined = append(unconfined, fmt.Sprintf(format, a...)) }
		if sb, err := buildSandbox(cfg, workspace); err == nil {
			defer func() { _ = sandbox.Close(sb) }()
			toolset.ConfineMCP(gw, cfg, sb, note)
		} else {
			toolset.ConfineMCP(gw, cfg, nil, note)
		}
		errs := gw.Connect(context.Background(), toolset.MCPConfigs(cfg))
		status := gw.Status()
		gw.Close()
		if len(status) > 0 {
			fmt.Printf("mcp         %s\n", strings.Join(status, ", "))
		} else {
			fmt.Printf("mcp         %d configured, none connected\n", len(servers))
		}
		// Why each that did not connect, as a session would warn of it.
		for _, err := range errs {
			fmt.Printf("            not connected: %v\n", err)
		}
		for _, w := range unconfined {
			fmt.Printf("            ⚠ %s\n", w)
		}
	}
	fmt.Println()

	// The workspace named a model that waits for trust: the default in its
	// place is not what this workspace would use, so it is not probed.
	if modelAwaitsTrust(cfg) {
		fmt.Println("endpoint    not probed: this workspace's model settings wait for trust, and the default in their place is not what it would use")
		fmt.Println("\nReview the workspace configuration with `abhed trust`, then run `abhed doctor` again.")
		return 1
	}
	adapter := buildAdapter(provider)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	fmt.Print("checking endpoint... ")
	stream, err := adapter.Complete(ctx, model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "Reply with the single word: ok"}},
		// Generous for a one-word answer, because a hybrid-reasoning model
		// spends this budget on its thinking phase FIRST. gemma4:26b returned
		// empty content and finish_reason=length at 32 tokens — a healthy
		// model reported as broken.
		MaxTokens: 512,
	})
	if err != nil {
		fmt.Printf("FAILED\n  %v\n", err)
		fmt.Println("\nCheck that the endpoint is reachable and the model name is correct.")
		return 1
	}
	var got strings.Builder
	for c := range stream {
		if c.Type == model.ChunkText {
			got.WriteString(c.Text)
		}
		if c.Type == model.ChunkError {
			fmt.Printf("FAILED\n  %v\n", c.Err)
			return 1
		}
	}
	answer := strings.TrimSpace(got.String())
	if answer == "" {
		// Distinguish "said nothing" from "said something unexpected": the
		// first usually means the token budget went to reasoning, which is a
		// configuration problem, not a broken endpoint.
		fmt.Printf("ok\n  response was empty — if this model reasons before " +
			"answering, raise context.max_tokens\n")
	} else {
		fmt.Printf("ok\n  response: %q\n", answer)
	}

	// Tool calling is the capability the agent actually depends on.
	fmt.Print("checking tool calling... ")
	stream, err = adapter.Complete(ctx, model.Request{
		Messages: []model.Message{{Role: model.RoleUser, Content: "List files matching *.go using the glob tool."}},
		Tools: []model.ToolDef{{
			Name:        "glob",
			Description: "Find files matching a glob pattern.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`),
		}},
		MaxTokens: 256,
	})
	if err != nil {
		fmt.Printf("FAILED\n  %v\n", err)
		return 1
	}
	calls := 0
	for c := range stream {
		if c.Type == model.ChunkToolCall {
			calls++
			fmt.Printf("ok\n  called %s with %s\n", c.ToolCall.Name, c.ToolCall.Args)
		}
	}
	if calls == 0 {
		fmt.Println("FAILED")
		fmt.Println("  The model did not emit a tool call. Abhed requires tool-calling support.")
		fmt.Println("  Check that the serving stack has a tool-call parser enabled for this model.")
		return 1
	}

	// The sandbox is checked by running something through it, not by asking
	// whether it is configured. Inside a hardened container bubblewrap could
	// not mount /proc and every command failed; doctor said "process — via
	// bwrap" and nothing else, because it never tried. Now it tries.
	fmt.Print("checking sandbox exec... ")
	if sb, err := buildSandbox(cfg, workspace); err != nil {
		// A session would not start either, so this is not ready.
		fmt.Printf("FAILED\n  %v\n", err)
		return 1
	} else {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := sb.Command(sctx, workspace, "echo abhed-sandbox-ok").CombinedOutput()
		cancel()
		if cerr := sandbox.Close(sb); cerr != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", cerr)
		}
		if err != nil || !strings.Contains(string(out), "abhed-sandbox-ok") {
			fmt.Println("FAILED")
			fmt.Printf("  tier %s could not run a command: %v\n", sb.Tier(), err)
			if msg := strings.TrimSpace(string(out)); msg != "" {
				fmt.Printf("  %s\n", msg)
			}
			fmt.Println("  The agent's bash tool would fail the same way. Fix the sandbox before relying on it.")
			return 1
		}
		fmt.Printf("ok\n  ran a command under the %s tier\n", sb.Tier())
	}

	if vaultErr != nil {
		fmt.Println("\nNot ready: the secrets store cannot be loaded (see above), so no session will start.")
		return 1
	}
	return doctorVerdict(os.Stdout, findings)
}

// limitWarnings names the configured limits the tier in force does not apply:
// memory is bounded on the container and vm tiers only, processes on all but
// none, and not for root on the process tier. A memory limit no file set, the
// default, is not warned about.
func limitWarnings(cfg config.Config, tier sandbox.Tier) []string {
	var out []string
	// The Linux process tier bounds each process's data, not the command's total.
	if m := cfg.Sandbox.MaxMemoryMB; m > 0 && cfg.Sets("sandbox.max_memory_mb") && tier.Strength() < sandbox.TierContainer.Strength() && tier != sandbox.TierFence &&
		(tier != sandbox.TierProcess || runtime.GOOS != "linux") {
		out = append(out, fmt.Sprintf("sandbox.max_memory_mb (%d) is not applied on the %s tier here; the container, vm and fence tiers bound memory, and the process tier on Linux each process's data", m, tier))
	}
	switch {
	case cfg.Sandbox.MaxProcs > 0 && tier == sandbox.TierNone:
		out = append(out, fmt.Sprintf("sandbox.max_procs (%d) is not applied on the none tier", cfg.Sandbox.MaxProcs))
	case cfg.Sandbox.MaxProcs > 0 && tier == sandbox.TierProcess && runningAsRoot():
		out = append(out, fmt.Sprintf("sandbox.max_procs (%d) is not applied: this runs as root, whose processes the kernel does not bound", cfg.Sandbox.MaxProcs))
	}
	return out
}

// internalModelEndpoints warns, under the allowlist, of each configured
// model endpoint on an internal address: the model client is Abhed's own and
// is not held to the address check commands are, so such a base_url (a cloud
// metadata address, say) is reached all the same.
func internalModelEndpoints(cfg config.Config) []string {
	if cfg.Sandbox.Network != config.NetworkAllowlist {
		return nil
	}
	names := make([]string, 0, len(cfg.Model.Providers))
	for name := range cfg.Model.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		if why := egress.InternalEndpoint(context.Background(), cfg.Model.Providers[name].BaseURL, nil); why != "" {
			out = append(out, fmt.Sprintf("provider %s: %s, which the model client reaches though the allowlist refuses it to commands; "+
				"make sure only trusted configuration sets it", config.Printable(name), why))
		}
	}
	return out
}

// pathDenyWarning warns, under the allowlist, of deny rules narrowed by a
// path, which a server's other spellings of the path step around.
func pathDenyWarning(cfg config.Config) string {
	if cfg.Sandbox.Network != config.NetworkAllowlist {
		return ""
	}
	pol, err := egress.Compile(cfg.Egress)
	if err != nil {
		return ""
	}
	names := pol.PathDenies()
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("%s deny by path, which a server may also serve as /ADMIN, /admin/ or /admin.json; "+
		"allow the paths commands need and let the default deny the rest (docs/guide/21-network-policy.md#path-rules)",
		strings.Join(names, ", "))
}

// egressLabel states the commands' network policy: off, open, or the
// allowlist with its rules and mode.
func egressLabel(cfg config.Config) string {
	if cfg.Sandbox.Network != config.NetworkAllowlist {
		if cfg.Sandbox.AllowNetwork {
			return "open — sandbox.allow_network lets commands reach any host directly"
		}
		return "off — commands have no network (sandbox.allow_network is false)"
	}
	pol, err := egress.Compile(cfg.Egress)
	if err != nil {
		return "UNAVAILABLE — " + err.Error()
	}
	def, mode := "deny", "enforce"
	if pol.DefaultAllow() {
		def = "allow"
	}
	if pol.Audit() {
		mode = "audit (denials recorded, not enforced)"
	}
	if pol.InspectTLS() {
		mode += ", HTTPS inspected with a CA per session"
	}
	return fmt.Sprintf("allowlist — %d rules, default %s, mode %s; a proxy on loopback per session, "+
		"each decision recorded as %s; direct sockets blocked on the process tier only", pol.Rules(), def, mode, sandbox.EvEgressDecision)
}

// printDrivers shows the sandbox drivers beyond the default, when the
// configuration names one, and reports whether one cannot be reached.
func printDrivers(w io.Writer, cfg config.Config) bool {
	if cfg.ControlPlane.PerSession {
		fmt.Fprintf(w, "sessions    each gets a sandbox: %s\n", perSessionLabel(cfg))
	}
	if cfg.Sandbox.MicroVMSocket == "" && !cfg.ControlPlane.Docker.Enabled && !cfg.ControlPlane.Podman.Enabled {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, entries, err := builtin.Registry(ctx, cfg)
	if err != nil {
		fmt.Fprintf(w, "drivers     UNAVAILABLE — %v\n", err)
		return true
	}
	down := false
	for _, e := range entries {
		if e.Name == builtin.Default {
			continue
		}
		if e.Err != nil {
			fmt.Fprintf(w, "%-11s UNAVAILABLE — %s: %v\n", e.Name, e.Detail, e.Err)
			down = true
			continue
		}
		fmt.Fprintf(w, "%-11s ready — %s; sandboxes may choose it by name, the default stays %s\n", e.Name, e.Detail, builtin.Default)
	}
	return down
}

// printFenceProbe shows the fence's probe, check by check, when the
// configuration chose the fence tier.
func printFenceProbe(w io.Writer, cfg config.Config, workspace string) {
	p, err := sandboxconfig.Policy(cfg, workspace)
	if err != nil {
		fmt.Fprintf(w, "fence       UNAVAILABLE — %v\n", err)
		return
	}
	lines := strings.Split(sandbox.FenceProbe(context.Background(), p).String(), "\n")
	fmt.Fprintf(w, "fence       preview · %s\n", lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(w, "          %s\n", l)
	}
}

// runningAsRoot is replaced in tests.
var runningAsRoot = func() bool { return os.Getuid() == 0 }

// configCheck is what the doctor found in the configuration's keys.
type configCheck struct {
	// unknown is a key nothing reads; notInEffect a key this version reads
	// but does not act on yet.
	unknown, notInEffect bool
	// rules is a permission rule that names no tool as written; neverManaged
	// a managed deny or ask rule among them, which the organisation relies on.
	rules, neverManaged bool
	// mcpOverlap is a pair of MCP servers whose tools' names overlap.
	mcpOverlap bool
	// storeDown and authDown report an event store or sign-in doctor could
	// not open, as before `abhed migrate`.
	storeDown, authDown bool
	// driverDown is a configured sandbox driver that cannot be reached.
	driverDown bool
	// tlsDown is server.tls that serve would refuse to start with.
	tlsDown bool
}

// configFindings lists both kinds of key and reports which there were.
func configFindings(w io.Writer, cfg config.Config) configCheck {
	unknown := printUnknown(w, cfg)
	c := configCheck{unknown: unknown, notInEffect: printNotInEffect(w, cfg)}
	c.rules, c.neverManaged = printRuleFindings(w, cfg)
	for _, m := range cfg.MCPServerOverlaps() {
		fmt.Fprintf(w, "mcp         %s  ⚠\n", m)
		c.mcpOverlap = true
	}
	return c
}

// printRuleFindings lists the permission rules that name no tool as written,
// and reports whether there were any, and whether a managed deny or ask rule is among them.
func printRuleFindings(w io.Writer, cfg config.Config) (found, neverManaged bool) {
	for i, f := range cfg.RuleFindings() {
		label := "            "
		if i == 0 {
			label = "rules       "
		}
		fmt.Fprintf(w, "%s%s  ⚠\n", label, f)
		found = true
		neverManaged = neverManaged || f.FailsDoctor()
	}
	return found, neverManaged
}

// doctorVerdict ends a doctor run whose checks all passed: ready, unless the
// configuration has keys nothing reads, or keys this version does not act
// on yet. Each is said as what it is.
func doctorVerdict(w io.Writer, f configCheck) int {
	if f == (configCheck{}) {
		fmt.Fprintln(w, "\nReady.")
		return 0
	}
	fmt.Fprintln(w)
	var down []string
	if f.storeDown {
		down = append(down, "the event store")
	}
	if f.authDown {
		down = append(down, "sign-in")
	}
	if f.driverDown {
		fmt.Fprintln(w, "Not ready: a configured sandbox driver cannot be reached (see above); sandboxes asking for it are refused.")
	}
	if len(down) > 0 {
		fmt.Fprintf(w, "Not ready: %s could not be opened (see above). If the database has no Abhed schema yet, run `abhed migrate`.\n",
			strings.Join(down, " and "))
	}
	if f.tlsDown {
		fmt.Fprintln(w, "Not ready: server.tls cannot be used (see above), so serve will not start.")
	}
	if f.unknown {
		fmt.Fprintln(w, "Not ready: the configuration has keys nothing reads (listed above). Correct or remove them.")
	}
	if f.notInEffect {
		fmt.Fprintln(w, "Not ready: the configuration sets keys this version does not act on yet (listed above), "+
			"so the controls they name are not in force. Remove them, or run a version that acts on them.")
	}
	if f.neverManaged {
		fmt.Fprintln(w, "Not ready: the managed configuration has a deny or ask rule that names no tool as written (listed above), "+
			"so the control it names is not in force here. Whoever manages it must correct the rule.")
	}
	if f.mcpOverlap {
		fmt.Fprintln(w, "Not ready: two MCP servers' names overlap (listed above), so one is not started. Rename one of them.")
	}
	if f == (configCheck{rules: true}) {
		// Only rule warnings nothing relies on, a person's own rules or a managed
		// allow rule: said above, not a failure.
		fmt.Fprintln(w, "Ready, with the rule warnings above.")
		return 0
	}
	return 1
}

// printNotInEffect lists the settings the files made that this version does
// not act on yet, and reports whether there were any.
func printNotInEffect(w io.Writer, cfg config.Config) bool {
	keys := cfg.NotYetInEffect()
	for i, k := range keys {
		label := "            "
		if i == 0 {
			label = "config      "
		}
		fmt.Fprintf(w, "%s%s  ⚠\n", label, config.NotYetInEffectMessage(k))
	}
	return len(keys) > 0
}

// printUnknown lists the configuration's unknown keys and reports whether there were any.
func printUnknown(w io.Writer, cfg config.Config) bool {
	for i, u := range cfg.Unknown {
		label := "            "
		if i == 0 {
			label = "config      "
		}
		fmt.Fprintf(w, "%s%s  ⚠\n", label, u)
	}
	return len(cfg.Unknown) > 0
}

// modelAwaitsTrust reports an untrusted workspace that sets the model while
// no trusted file or ABHED_BASE_URL does.
func modelAwaitsTrust(cfg config.Config) bool {
	if cfg.Sets("model") || os.Getenv("ABHED_BASE_URL") != "" {
		return false
	}
	for _, k := range cfg.Workspace.Ignored {
		if k.Key == "model" || strings.HasPrefix(k.Key, "model.") {
			return true
		}
	}
	return false
}

// modeLabel is the permission mode, and when it is bypass under a managed
// file, that the file disables it, so a call asks as in default mode.
func modeLabel(cfg config.Config) string {
	mode := orDefault(cfg.Permissions.Mode, "default")
	if line := bypassDisabled(cfg); line != "" {
		return mode + " (" + line + ")"
	}
	return mode
}

// bypassDisabled says that the managed file disables bypass mode, when the
// mode is bypass under one; "" otherwise.
func bypassDisabled(cfg config.Config) string {
	if !cfg.Managed || cfg.Permissions.Mode != "bypass" {
		return ""
	}
	return "bypass disabled by " + config.Printable(managed.ConfigFile) + "; calls ask as in default mode"
}

// managedLine names the managed file in force and how much it sets, for the
// doctor and the serve banner, so an operator knows where a locked setting
// comes from; "" with none.
func managedLine(cfg config.Config) string {
	if !cfg.Managed {
		return ""
	}
	return fmt.Sprintf("%s (sets %d setting(s))", config.Printable(managed.ConfigFile), len(cfg.ManagedKeys))
}

// sessionLine says how long a browser sign-in lasts, when one does.
func sessionLine(cfg config.Config) string {
	h := cfg.Auth.SessionHours
	switch {
	case cfg.Auth.Mode != "local" && cfg.Auth.Mode != "oidc":
		return ""
	case h == 0 && cfg.Auth.Mode == "local":
		h = int(auth.DefaultSessionTTL.Hours())
	case h == 0:
		return ""
	}
	return fmt.Sprintf("a sign-in lasts %d hours (auth.session_hours)", h)
}

// tierLabel is the sandbox's tier, with what it lacks here: "process (no /proc)".
func tierLabel(sb sandbox.Sandbox) string {
	if c := sandbox.Caveat(sb); c != "" {
		return string(sb.Tier()) + " (" + c + ")"
	}
	return string(sb.Tier())
}

// configFilesLine names the configuration files this run read, lowest layer
// first, so a doctor run from a subfolder says which files it judged.
func configFilesLine(cfg config.Config) string {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		if user := filepath.Join(home, ".abhed", "config.json"); fileExists(user) {
			files = append(files, user)
		}
	}
	if cfg.Workspace.File != "" && fileExists(cfg.Workspace.File) {
		files = append(files, cfg.Workspace.File)
	}
	if cfg.Settings.Name != "" {
		files = append(files, "-settings "+cfg.Settings.Name)
	}
	if cfg.Managed {
		files = append(files, managed.ConfigFile+" (managed)")
	}
	if len(files) == 0 {
		return "none; the built-in defaults"
	}
	return config.Printable(strings.Join(files, ", "))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// gitHeldLabel says whether commands are kept from writing git's files on
// tier, the one in force, or "" when it is not known.
func gitHeldLabel(cfg config.Config, tier sandbox.Tier) string {
	switch on, required := cfg.Sandbox.GitProtected(); {
	case !on:
		return "not held: sandbox.protect_git is false (docs/guide/04-permissions.md)"
	case required:
		return "held read-only from commands; a tier that cannot hold them is refused"
	case tier == sandbox.TierNone:
		return "not held: the sandbox tier is none, so commands can write git's config and hooks; make a stronger tier available (sandbox.protect_git)"
	}
	return "held read-only from commands on every tier but none (sandbox.protect_git)"
}

// confinedStdioServers reports whether an enabled stdio MCP server runs in
// the sandbox, not with the host's files.
func confinedStdioServers(cfg config.Config) bool {
	for _, s := range cfg.MCP.Servers {
		if s.Enabled && s.Command != "" && !s.HostFiles {
			return true
		}
	}
	return false
}
