package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/customcmd"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// Control (docs/architecture/studio-acp-contract.md §5): modes, the policy
// view and its dry run, diffs on asks, and workspace trust. Studio can read
// all of it and change only the mode, within the ceiling the engine enforces.

func init() {
	liveFeatures = append(liveFeatures, "modes", "policy.explain", "trust.inspect", "capabilities", "mcp.restart")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/mcp/restart":    (*acpConn).mcpRestart,
		"session/set_mode":      (*acpConn).setMode,
		"_abhed/policy/explain": (*acpConn).explain,
		"_abhed/capabilities":   (*acpConn).capabilities,
		"_abhed/trust/inspect":  (*acpConn).trustInspect,
	})
}

// allModes is every permission mode, in the order Studio lists them.
var allModes = []policy.Mode{policy.ModeDefault, policy.ModeAcceptEdits, policy.ModePlan, policy.ModeAuto, policy.ModeBypass}

var modeNames = map[policy.Mode][2]string{
	policy.ModeDefault:     {"Default", "Asks before every change and command"},
	policy.ModeAcceptEdits: {"Accept edits", "Edits in the workspace go ahead; commands still ask"},
	policy.ModePlan:        {"Plan", "Reads and plans; changes nothing until a plan is accepted"},
	policy.ModeAuto:        {"Auto", "Approves reads and workspace edits by rule; commands no rule covers still ask"},
	policy.ModeBypass:      {"Bypass", "Runs without asking; deny rules and the sandbox still hold"},
}

// availableModes are the modes the engine allows this session now: the
// managed ceiling applied, and bypass only where the person's own
// configuration chose it at the start.
func availableModes(cfg config.Config) []policy.Mode {
	var out []policy.Mode
	for _, m := range allModes {
		if m == policy.ModeBypass && cfg.Permissions.Mode != string(policy.ModeBypass) {
			continue
		}
		if _, err := cfg.Apply(config.Overrides{Mode: string(m)}); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// modeState is the spec's SessionModeState, nil for a session without a policy engine.
func (c *acpConn) modeState(s *acpSession) map[string]any {
	if !s.inner {
		return nil
	}
	cfg := s.parts.Config
	locked := cfg.ManagedSets("permissions.mode")
	modes := []any{}
	for _, m := range availableModes(cfg) {
		meta := map[string]any{"widens": m == policy.ModeAcceptEdits || m == policy.ModeAuto || m == policy.ModeBypass}
		if locked {
			meta["locked"], meta["lockedBy"] = true, "managed"
		}
		modes = append(modes, map[string]any{"id": string(m), "name": modeNames[m][0], "description": modeNames[m][1],
			"_meta": map[string]any{acpMetaKey: meta}})
	}
	return map[string]any{"currentModeId": string(s.parts.Loop.Policy.Mode), "availableModes": modes}
}

// modeConfigOption is the same list as a config option of category mode.
func modeConfigOption(state map[string]any) map[string]any {
	opts := []any{}
	for _, m := range state["availableModes"].([]any) {
		mm := m.(map[string]any)
		opts = append(opts, map[string]any{"value": mm["id"], "name": mm["name"], "description": mm["description"]})
	}
	return map[string]any{"id": "mode", "name": "Mode", "description": "The permission mode",
		"category": "mode", "type": "select", "currentValue": state["currentModeId"], "options": opts}
}

// configOptions are every config option of a session.
func (c *acpConn) configOptions(s *acpSession) []any {
	var opts []any
	if m, ok := s.agent.(modelSwitcher); ok {
		if models := m.Models(); len(models) > 0 {
			opts = append(opts, modelConfigOptions(models)...)
		}
	}
	if state := c.modeState(s); state != nil {
		opts = append(opts, modeConfigOption(state))
	}
	return opts
}

func (c *acpConn) setMode(msg rpcMessage) {
	var p struct {
		ModeID string `json:"modeId"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if e := c.changeMode(s, p.ModeID); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

// changeMode moves a session to a mode the engine allows now. Studio's
// own confirmation is not relied on: the ceiling is checked here.
func (c *acpConn) changeMode(s *acpSession, id string) *rpcError {
	parts, e := innerOf(s)
	if e != nil {
		return e
	}
	mode := policy.Mode(id)
	if !slices.Contains(availableModes(parts.Config), mode) {
		return refusal(errPolicy, "the mode %q is not available in this session", ui.VisibleLine(id))
	}
	// The policy engine is read by every call; it changes only between runs.
	if e := idle(s); e != nil {
		return e
	}
	if s.liveTasks() > 0 {
		return refusal(errBusy, "background tasks are running in this session; change the mode once they end")
	}
	from := parts.Loop.Policy.Mode
	if from == mode {
		return nil
	}
	parts.Loop.Policy.Mode = mode
	s.record(agent.EvModeChanged, agent.ModeChanged{From: string(from), To: string(mode), By: agent.ByUser, Via: agent.ViaStudio})
	return nil
}

func (c *acpConn) explain(msg rpcMessage) {
	var p struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if p.Tool == "" {
		c.reply(msg.ID, nil, refusal(errParams, "tool is required"))
		return
	}
	if len(p.Args) == 0 {
		p.Args = json.RawMessage(`{}`)
	}
	mutates := true
	if t, ok := parts.Loop.Tools.Get(p.Tool); ok {
		mutates = t.Mutates()
	}
	// A dry run: no hook is consulted, nothing is recorded, and the
	// session's "always" scopes are neither read nor changed.
	dry := *parts.Loop.Policy
	dry.Hooks, dry.EngineHooks = nil, nil
	d := dry.Evaluate(p.Tool, mutates, p.Args)
	res := map[string]any{"decision": string(d.Decision), "step": d.Step, "reason": redacted(d.Reason)}
	if d.Rule != "" {
		res["rule"] = redacted(d.Rule)
	}
	if scope := d.Offer(); scope != "" {
		res["scope"] = redacted(scope)
	}
	c.reply(msg.ID, res, nil)
}

// redacted replaces stored secret values in text shown to Studio.
func redacted(s string) string {
	raw, _ := json.Marshal(s)
	var out string
	if json.Unmarshal(openVault().Redactor().Redact(raw), &out) != nil {
		return agent.Withheld
	}
	return out
}

// policyView is the session's mode, sandbox and every rule with its layer
// and whether it applies, for Studio's policy view (§5.4). A configured
// rule's layer is the one loading credited it with (config.RuleLayer), as
// /permissions shows it; the session's own rules are "session".
func policyView(s *acpSession) map[string]any {
	cfg, pol := s.parts.Config, s.parts.Loop.Policy
	sandbox := map[string]any{"tier": orDefault(cfg.Sandbox.MinTier, "process"), "network": cfg.Sandbox.AllowNetwork}
	if t, ok := s.parts.Loop.Tools.Get("bash"); ok {
		if b, ok := t.(tools.Bash); ok && b.Isolation.Tier != "" {
			sandbox["tier"] = b.Isolation.Tier
			if b.Isolation.Backend != "" {
				sandbox["backend"] = b.Isolation.Backend
			}
		}
	}
	rules := []any{}
	rule := func(decision, text, layer string) map[string]any {
		return map[string]any{"decision": decision, "rule": redacted(text), "layer": layer, "applied": true}
	}
	for _, l := range []struct {
		name  string
		rules []policy.Rule
	}{{"deny", pol.Deny}, {"ask", pol.Ask}, {"allow", pol.Allow}} {
		for _, r := range l.rules {
			rules = append(rules, rule(l.name, r.String(), cfg.RuleLayer(l.name, r.String())))
		}
	}
	if pol.Session != nil {
		deny, ask, allow := pol.Session.SessionRules()
		for _, l := range []struct {
			name  string
			rules []string
		}{{"deny", deny}, {"ask", ask}, {"allow", allow}} {
			for _, r := range l.rules {
				rules = append(rules, rule(l.name, r, "session"))
			}
		}
		pinned, why := pol.Session.PinnedRules()
		for i, r := range pinned {
			v := rule("deny", r, "session")
			v["note"] = ui.VisibleLine(why[i])
			rules = append(rules, v)
		}
	}
	// What a workspace file not trusted asked for, shown as not applied.
	for _, k := range cfg.Workspace.Ignored {
		list, ok := strings.CutPrefix(k.Key, "permissions.")
		if !ok || (list != "allow" && list != "ask" && list != "deny") || k.Value == "" {
			continue
		}
		values := []string{k.Value}
		var many []string
		if json.Unmarshal([]byte(k.Value), &many) == nil {
			values = many
		}
		for _, v := range values {
			rules = append(rules, map[string]any{"decision": list, "rule": redacted(v), "layer": config.LayerWorkspace,
				"applied": false, "ignoredBecause": "workspace-untrusted"})
		}
	}
	// Allow rules a file added that the managed configuration's lock left out.
	for _, k := range cfg.SetAside {
		if list, ok := strings.CutPrefix(k.Key, "permissions."); ok && k.Value != "" {
			rules = append(rules, map[string]any{"decision": list, "rule": redacted(k.Value),
				"layer": setAsideLayer(k, cfg.Workspace.File), "applied": false, "ignoredBecause": "managed-override"})
		}
	}
	for _, b := range builtinRules {
		rules = append(rules, map[string]any{"decision": b[0], "rule": "builtin:" + b[1], "layer": "builtin", "applied": true})
	}
	return map[string]any{"mode": string(pol.Mode), "sandbox": sandbox, "managed": cfg.Managed, "rules": rules}
}

// setAsideLayer is the layer of the file a set-aside setting came from. The
// layer recorded when it was set aside wins: with the home folder as the
// workspace the user's file and the workspace's are one path.
func setAsideLayer(k config.SetAsideKey, workspaceFile string) string {
	if k.Layer != "" {
		return k.Layer
	}
	if workspaceFile != "" && k.File == workspaceFile {
		if home, err := os.UserHomeDir(); err != nil || k.File != filepath.Join(home, ".abhed", "config.json") {
			return config.LayerWorkspace
		}
	}
	return config.LayerUser
}

// builtinRules are the engine's own steps that decide before any rule.
var builtinRules = [][2]string{
	{"deny", "state"}, {"deny", "editor-files"}, {"ask", "destructive"}, {"ask", "screen"}, {"deny", "screen"},
}

// askDiff is the change an edit or write would make, for the ask that
// precedes it (§5.5).
type askChange struct {
	content   []any
	locations []any
	meta      []any
	hunksOnly bool
}

// diffWhole bounds a file sent whole in an ask; a larger one sends its changed region.
const diffWhole = 256 << 10

func (s *acpSession) askDiff(tool string, args json.RawMessage) *askChange {
	if !s.inner || (tool != "edit" && tool != "write") {
		return nil
	}
	var a struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if json.Unmarshal(args, &a) != nil || a.Path == "" {
		return nil
	}
	path, err := s.parts.Session.Resolve(a.Path)
	if err != nil {
		return nil
	}
	before, err := s.parts.Session.ReadFile(path)
	existed := err == nil
	old := string(before)
	var next string
	switch tool {
	case "write":
		next = a.Content
	case "edit":
		switch {
		case !existed && a.OldString == "":
			next = a.NewString
		case a.ReplaceAll:
			next = strings.ReplaceAll(old, a.OldString, a.NewString)
		default:
			next = strings.Replace(old, a.OldString, a.NewString, 1)
		}
	}
	// The file on disk may hold a stored secret; Studio is shown it redacted.
	old, next = redacted(old), redacted(next)
	hunks := len(old) > diffWhole || len(next) > diffWhole
	if hunks {
		old, next = changedRegion(old, next, 3)
	}
	d := map[string]any{"type": "diff", "path": path, "newText": next}
	m := map[string]any{"path": path, "newText": next}
	if existed {
		d["oldText"], m["oldText"] = old, old
	}
	return &askChange{content: []any{d}, locations: []any{map[string]any{"path": path}}, meta: []any{m}, hunksOnly: hunks}
}

// changedRegion cuts two texts to the lines that differ, with context lines
// either side.
func changedRegion(a, b string, context int) (string, string) {
	al, bl := strings.SplitAfter(a, "\n"), strings.SplitAfter(b, "\n")
	pre := 0
	for pre < len(al) && pre < len(bl) && al[pre] == bl[pre] {
		pre++
	}
	suf := 0
	for suf < len(al)-pre && suf < len(bl)-pre && al[len(al)-1-suf] == bl[len(bl)-1-suf] {
		suf++
	}
	start := max(0, pre-context)
	ae, be := min(len(al), len(al)-suf+context), min(len(bl), len(bl)-suf+context)
	return strings.Join(al[start:ae], ""), strings.Join(bl[start:be], "")
}

// editorFiles are the paths in a workspace an editor reads as its own
// configuration; the agent may not change them (§2.6).
var editorFiles = []string{".vscode", ".devcontainer", ".git/config", ".git/hooks"}

// protectedPaths are a workspace's editor files as paths for the sandbox, from
// the workspace as given and resolved, with the *.code-workspace files that
// exist now and the git folders a .git file points to.
func protectedPaths(ws string) []string {
	var out []string
	for _, root := range sandbox.PathForms(ws) {
		for _, f := range editorFiles {
			out = append(out, filepath.Join(root, filepath.FromSlash(f)))
		}
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if codeWorkspace(e.Name()) {
					out = append(out, filepath.Join(root, e.Name()))
				}
			}
		}
		for _, dir := range gitDirs(root) {
			out = append(out, filepath.Join(dir, "config"), filepath.Join(dir, "hooks"))
		}
		out = append(out, nestedGit(root)...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Bounds on the search for git repositories nested in a workspace.
const (
	nestedDepth   = 6
	nestedRepos   = 64
	nestedEntries = 20000
)

// nestedGit are the config and hooks of the git repositories nested in root
// when the session starts, within the bounds above; a repository made later
// is held by pattern on macOS only.
func nestedGit(root string) []string {
	var out []string
	repos, seen := 0, 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil //nolint:nilerr // an unreadable folder is passed by
		}
		if seen++; seen > nestedEntries || repos >= nestedRepos {
			return filepath.SkipAll
		}
		name := d.Name()
		if !d.IsDir() && !strings.EqualFold(name, ".git") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		depth := len(strings.Split(rel, string(filepath.Separator)))
		switch {
		case strings.EqualFold(name, ".git"):
			if dir := filepath.Dir(p); dir != root {
				repos++
				if d.IsDir() {
					out = append(out, filepath.Join(p, "config"), filepath.Join(p, "hooks"))
				}
				for _, g := range gitDirs(dir) {
					out = append(out, filepath.Join(g, "config"), filepath.Join(g, "hooks"))
				}
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
		case strings.EqualFold(name, tools.StateDir), name == "node_modules", depth >= nestedDepth:
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

// codeWorkspace reports whether name is a VS Code workspace file, in any case.
func codeWorkspace(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".code-workspace")
}

// gitDirs are the git folders a .git file in root points to: a worktree's or
// submodule's own, and the common folder it shares.
func gitDirs(root string) []string {
	dir := gitFileTarget(filepath.Join(root, ".git"))
	if dir == "" {
		return nil
	}
	out := []string{dir}
	if common := readPointer(filepath.Join(dir, "commondir"), ""); common != "" {
		out = append(out, common)
	}
	return out
}

// gitFileTarget is the folder a .git file names with "gitdir:", or "".
func gitFileTarget(p string) string {
	return readPointer(p, "gitdir:")
}

// readPointer reads a small file holding one path after prefix, resolved
// against the file's folder.
func readPointer(p, prefix string) string {
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return ""
	}
	b, err := os.ReadFile(p) // #nosec G304 -- a .git pointer file in the workspace
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	return filepath.Clean(target)
}

// editorFile reports whether path is one of the editor's own files in one of
// roots, or the .git that holds some of them. Links are followed and names
// compared without case, as APFS and NTFS compare them.
func editorFile(path string, roots, protected []string) bool {
	forms := sandbox.PathForms(path)
	for _, p := range forms {
		if codeWorkspace(filepath.Base(p)) {
			return true
		}
		// Those found at the start, such as a git folder a nested .git file names.
		for _, q := range protected {
			if _, ok := sandbox.Within(p, q); ok {
				return true
			}
		}
	}
	for _, root := range roots {
		for _, r := range sandbox.PathForms(root) {
			for _, p := range forms {
				if rest, ok := sandbox.Within(p, r); ok && editorRest(rest) {
					return true
				}
			}
			for _, p := range forms {
				if rest, ok := sandbox.Within(p, r); ok && namedByGitFile(r, rest, forms) {
					return true
				}
			}
		}
	}
	return false
}

// namedByGitFile reports whether a path is the git folder, config or hooks a
// .git file in root or a folder on the way to the path names.
func namedByGitFile(root string, rest, forms []string) bool {
	dir := root
	for i := 0; i <= len(rest); i++ {
		for _, g := range gitDirs(dir) {
			for _, d := range sandbox.PathForms(g) {
				for _, p := range forms {
					if r, ok := sandbox.Within(p, d); ok && (len(r) == 0 || gitOwn(r)) {
						return true
					}
				}
			}
		}
		if i < len(rest) {
			dir = filepath.Join(dir, rest[i])
		}
	}
	return false
}

// editorRest reports whether a path's parts below a root name an editor file.
func editorRest(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	if strings.EqualFold(rest[0], ".vscode") || strings.EqualFold(rest[0], ".devcontainer") {
		return true
	}
	// Any .git, at any depth, and its config and hooks: the editor's git runs
	// in nested repositories too.
	for i, part := range rest {
		if strings.EqualFold(part, ".git") && (i == len(rest)-1 || gitOwn(rest[i+1:])) {
			return true
		}
	}
	return false
}

// gitOwn reports whether parts below a git folder name its config or hooks.
func gitOwn(rest []string) bool {
	return len(rest) > 0 && (strings.EqualFold(rest[0], "config") || strings.EqualFold(rest[0], "hooks"))
}

// dirtyGuard refuses an edit to a file with unsaved Studio changes, or to an
// editor file; roots come from the tools session, copied under its lock.
func (s *acpSession) dirtyGuard(path string, roots []string) error {
	if editorFile(path, roots, s.protected) {
		return errEditorFile
	}
	s.mu.Lock()
	dirty := s.dirty[bufferKey(tools.RealPath(path))]
	s.mu.Unlock()
	if dirty {
		return errDirtyBuffer
	}
	return nil
}

// bufferKey is a resolved path as the dirty set keys it: without case where
// the filesystem ignores it, so draft.md and Draft.md are one file.
func bufferKey(real string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(real)
	}
	return real
}

type guardError string

func (e guardError) Error() string { return string(e) }

const (
	errEditorFile  guardError = "this file is the editor's own configuration; the agent may not change it"
	errDirtyBuffer guardError = "the person has unsaved changes to this file"
	// errEditorFileUndo is errEditorFile for the person's reject or undo, which
	// Abhed does not write to an editor file either.
	errEditorFileUndo guardError = "this file is the editor's own configuration; Abhed does not write it for a reject or an undo either, so change it in the editor"
)

// roots are the session's workspace and added directories.
func (s *acpSession) roots() []string {
	if !s.inner {
		return []string{s.cwd}
	}
	return append([]string{s.parts.Session.Root}, s.parts.Session.Roots...)
}

// capabilities is what this session can reach (§6.4): read-only and redacted.
func (c *acpConn) capabilities(msg rpcMessage) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.checkTrust(s)
	cfg, loop, set := parts.Config, parts.Loop, parts.Set
	prof := loop.Adapter.Profile()
	out := map[string]any{
		"model":  map[string]any{"name": prof.Name, "context_window": prof.ContextWindow},
		"policy": policyView(s),
	}
	toolList := []any{}
	for _, t := range loop.Tools.All() {
		ct := map[string]any{"name": t.Name(), "description": firstSentence(t.Description()), "mutates": t.Mutates(), "source": "builtin"}
		if rest, ok := strings.CutPrefix(t.Name(), "mcp__"); ok {
			server, _, _ := strings.Cut(rest, "__")
			ct["source"], ct["server"] = "mcp", server
		} else if t.Name() == "skill" {
			ct["source"] = "skill"
		}
		toolList = append(toolList, ct)
	}
	out["tools"] = toolList
	out["agents"] = agentsView(cfg, set)
	out["skills"] = skillsView(set)
	out["mcp"] = mcpView(cfg, set)
	ext := []any{}
	status := toolset.ExtensionStatus(cfg, set.Extensions)
	for _, x := range cfg.Extensions {
		ext = append(ext, map[string]any{"name": x.Name, "events": orEmptyList(x.Events), "status": orDefault(status[x.Name], "not started"),
			"source": cfg.ExtensionLayer(x.Name)})
	}
	out["extensions"] = ext
	out["web"] = map[string]any{"search": cfg.WebSearch.Enabled, "fetch": cfg.WebFetch.Enabled,
		"allowed_hosts": orEmptyList(cfg.WebFetch.AllowedHosts), "ask": cfg.WebFetch.Enabled && len(cfg.WebFetch.AllowedHosts) == 0}
	clusters := []any{}
	for _, cl := range cfg.K8s.Clusters {
		clusters = append(clusters, map[string]any{"name": cl.Name, "context": cfg.K8s.Context, "logged_in": false})
	}
	hosts := []string{}
	for _, h := range cfg.SSH.Hosts {
		hosts = append(hosts, h.Name)
	}
	out["infra"] = map[string]any{"clusters": clusters, "ssh_hosts": hosts}
	secretNames := []any{}
	if names, err := openVault().Names(); err == nil {
		for _, n := range names {
			secretNames = append(secretNames, map[string]any{"name": n})
		}
	}
	out["secrets"] = secretNames
	out["index"] = map[string]any{"enabled": set.Index != nil}
	rag := []any{}
	for _, r := range cfg.RAG.Corpora {
		if r.Enabled {
			rag = append(rag, map[string]any{"name": r.Name, "docs": 0})
		}
	}
	out["rag"] = rag
	out["memory"] = memoryView(cfg, loop.Policy, s.cwd)
	out["memory_auto"] = cfg.Memory.Auto
	out["studio"] = map[string]any{"host_terminal": !cfg.Studio.DisableHostTerminal}
	c.reply(msg.ID, out, nil)
}

func orEmptyList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func agentsView(cfg config.Config, set *toolset.Set) []any {
	out := []any{}
	defs := set.Agents
	for _, name := range defs.Names() {
		d, ok := defs.Get(name)
		if !ok {
			continue
		}
		v := map[string]any{"name": d.Name, "source": agentSource(d.Source), "tools": orEmptyList(d.Tools),
			"description": d.Description, "trusted": true}
		if d.SHA256 != "" {
			v["sha256"] = d.SHA256
		}
		if d.Model != "" {
			v["model"] = d.Model
		}
		if d.Path != "" {
			v["path"] = d.Path
		}
		out = append(out, v)
	}
	// The workspace's definitions not trusted are listed, refused, so Studio can say why.
	if !cfg.Workspace.AgentsTrusted {
		for _, rel := range cfg.Workspace.Agents {
			out = append(out, map[string]any{"name": strings.TrimSuffix(filepath.Base(rel), ".md"), "source": "workspace",
				"tools": []string{}, "trusted": false, "refused": "the workspace's agent definitions are not trusted (" + cfg.Workspace.AgentsReason + ")",
				"path": rel})
		}
	}
	return out
}

func agentSource(s string) string {
	switch s {
	case "builtin", "managed", "workspace":
		return s
	}
	return "user"
}

func skillsView(set *toolset.Set) []any {
	out := []any{}
	if set.Skills == nil {
		return out
	}
	for _, sk := range set.Skills.All() {
		out = append(out, map[string]any{"name": sk.Name, "description": sk.Description, "source": "user",
			"has_pipeline": sk.Pipeline != nil, "dir": sk.Dir, "trusted": true})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["name"].(string) < out[j].(map[string]any)["name"].(string)
	})
	return out
}

func mcpView(cfg config.Config, set *toolset.Set) []any {
	live := map[string]mcp.ServerStatus{}
	if set.Gateway != nil {
		for _, st := range set.Gateway.Servers() {
			live[st.Name] = st
		}
	}
	toolsOf := func(server string) []string {
		out := []string{}
		if set.Registry == nil {
			return out
		}
		for _, t := range set.Registry.All() {
			if rest, ok := strings.CutPrefix(t.Name(), "mcp__"+server+"__"); ok {
				out = append(out, rest)
			}
		}
		return out
	}
	out := []any{}
	for _, m := range cfg.MCP.Servers {
		if !m.Enabled || !mcp.ValidServerName(m.Name) {
			continue
		}
		transport, status := "stdio", "error"
		if m.URL != "" {
			transport = "http"
		}
		v := map[string]any{"name": m.Name, "transport": transport, "tools": toolsOf(m.Name), "source": "user", "pinned": m.Digest != ""}
		st, known := live[m.Name]
		switch {
		case known && st.Connected:
			status = "connected"
		case known && st.Err != nil:
			v["error"] = mcpError(st.Err)
		}
		v["status"] = status
		if refused := refusedNames(st.Refused); len(refused) > 0 {
			v["refusedTools"] = refused
		}
		out = append(out, v)
	}
	return out
}

// mcpError is a server's connection error as a person may read it: redacted
// and shown on one line.
func mcpError(err error) string {
	return ui.VisibleLine(redacted(err.Error()))
}

// refusedNames are the tool names a server offered that were not
// registered, shown as text with every hidden character marked; at most 20,
// each cut to 100 characters.
func refusedNames(names []string) []string {
	var out []string
	for _, n := range names {
		if len(out) == 20 {
			break
		}
		if r := []rune(n); len(r) > 100 {
			n = string(r[:100]) + "…"
		}
		out = append(out, ui.VisibleLine(n))
	}
	return out
}

// mcpRestart is _abhed/mcp/restart {sessionId, name} (§6.4): the person
// reconnects one configured MCP server. It is recorded mcp.status by: user,
// and refused while a prompt runs, since the session's tools share the
// connection.
func (c *acpConn) mcpRestart(msg rpcMessage) {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e == nil {
		e = claimRestart(s)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	// Released before each reply, so a client's next call never finds the
	// session still held by this restart.
	release := restartRelease(s)
	defer release() // a panicking restart still lets the session go
	known := false
	for _, m := range parts.Config.MCP.Servers {
		known = known || (m.Enabled && m.Name == p.Name)
	}
	if p.Name == "" || !known || parts.Set == nil || parts.Set.Gateway == nil {
		release()
		c.reply(msg.ID, nil, refusal(errParams, "no MCP server of that name is configured and enabled for this session"))
		return
	}
	ctx, cancel := context.WithTimeout(c.root(), mcpRestartTimeout)
	defer cancel()
	ev := agent.MCPStatus{Server: p.Name, Op: "restart", Status: "connected", By: agent.ByUser}
	if err := parts.Set.Gateway.Restart(ctx, p.Name); err != nil {
		ev.Status, ev.Error = "error", mcpError(err)
	}
	s.record(agent.EvMCPStatus, ev)
	release()
	out := map[string]any{"status": ev.Status}
	if ev.Error != "" {
		out["error"] = ev.Error
	}
	c.reply(msg.ID, out, nil)
}

// restartRelease lets the session go from a restart's claim. It acts once,
// so a deferred call cannot clear a later restart's claim.
func restartRelease(s *acpSession) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.mcpRestart = false
			s.mu.Unlock()
		})
	}
}

// claimRestart marks the session busy for an MCP restart, or says why not:
// a prompt, a woken turn or another restart holds it.
func claimRestart(s *acpSession) *rpcError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil || s.woken != nil || s.mcpRestart {
		return refusal(errBusy, "the session is busy (a prompt, a woken turn or another restart); try again when it ends")
	}
	s.mcpRestart = true
	return nil
}

// mcpRestartTimeout bounds how long a restart waits for the server to answer.
const mcpRestartTimeout = 30 * time.Second

func memoryView(cfg config.Config, pol *policy.Engine, ws string) []any {
	out := []any{}
	for _, f := range agent.LoadMemory(memoryOptions(cfg, pol, ws)).Files() {
		var size int64
		if info, err := os.Stat(f.Path); err == nil {
			size = info.Size()
		}
		scope := "user"
		switch f.Scope {
		case "managed":
			scope = "managed"
		case "project", "local", "subdirectory", "rule":
			scope = "workspace"
		case "auto":
			scope = "auto"
		}
		out = append(out, map[string]any{"path": f.Path, "scope": scope, "bytes": size, "loaded": true})
	}
	return out
}

// trustInspect is what the workspace's configuration file would bring if
// it were trusted, for Studio's review before a person runs abhed trust grant.
// There is no grant method (§5.6).
func (c *acpConn) trustInspect(msg rpcMessage) {
	var p struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	if p.Cwd == "" {
		p.Cwd = c.base
	}
	st, err := config.InspectWorkspace(p.Cwd)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the workspace could not be inspected: %v", err))
		return
	}
	out := map[string]any{}
	raw, _ := json.Marshal(st)
	_ = json.Unmarshal(raw, &out)
	agents := []any{}
	for _, f := range st.AgentFiles() {
		agents = append(agents, map[string]any{"name": strings.TrimSuffix(filepath.Base(f.Rel), ".md"), "sha256": config.HashOf(f.Data)})
	}
	out["agents"] = agents
	cmds := []any{}
	if files, _, _ := customcmd.ReadWorkspace(p.Cwd); len(files) > 0 {
		for _, f := range files {
			cmds = append(cmds, map[string]any{"name": "/" + strings.TrimSuffix(f.Rel, ".md"), "sha256": config.HashOf(f.Data)})
		}
	}
	out["commands"] = cmds
	skills, servers := []any{}, []any{}
	if st.File != "" {
		if data, err := os.ReadFile(st.File); err == nil { // #nosec G304 -- the workspace file InspectWorkspace found
			var f struct {
				Skills struct {
					Dirs []string `json:"dirs"`
				} `json:"skills"`
				MCP struct {
					Servers []struct {
						Name    string `json:"name"`
						Command string `json:"command"`
						URL     string `json:"url"`
					} `json:"servers"`
				} `json:"mcp"`
			}
			if json.Unmarshal(data, &f) == nil {
				for _, d := range f.Skills.Dirs {
					dir := d
					if !filepath.IsAbs(dir) {
						dir = filepath.Join(p.Cwd, dir)
					}
					found, _ := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
					for _, sk := range found {
						skills = append(skills, map[string]any{"name": filepath.Base(filepath.Dir(sk)), "dir": filepath.Dir(sk)})
					}
				}
				for _, m := range f.MCP.Servers {
					if !mcp.ValidServerName(m.Name) {
						continue
					}
					v := map[string]any{"name": m.Name}
					if m.Command != "" {
						v["command"] = m.Command
					}
					if m.URL != "" {
						v["url"] = m.URL
					}
					servers = append(servers, v)
				}
			}
		}
	}
	out["skills"], out["mcp"] = skills, servers
	c.reply(msg.ID, out, nil)
}

// checkTrust tells Studio when the workspace file's bytes changed since the
// session opened; the session restarts under the new decision at its next prompt.
func (c *acpConn) checkTrust(s *acpSession) bool {
	st, err := config.InspectWorkspace(s.cwd)
	if err != nil {
		return false
	}
	s.mu.Lock()
	old := s.trustSHA
	changed := st.SHA256 != old
	if changed {
		s.trustSHA = st.SHA256
	}
	s.mu.Unlock()
	if !changed {
		return false
	}
	c.notification("_abhed/trust/changed", map[string]any{"cwd": s.cwd, "oldSha256": old, "newSha256": st.SHA256})
	return true
}
