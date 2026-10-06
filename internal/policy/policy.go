// Package policy implements Abhed's permission model.
//
// Evaluation is ordered (docs P7):
//
//	Arguments → Hooks → Deny rules → Ask rules → Permission mode → Allow rules → Callback
//
// Deny is absolute: a matching deny rule blocks the tool even in the most
// permissive mode. Rules are scoped per-command, not per-tool, so allowing
// `bash(npm test)` never allows `bash(rm -rf /)`.
package policy

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/zybuu-ai/abhed/internal/kubescope"
	"github.com/zybuu-ai/abhed/internal/tools"
)

type Mode string

const (
	ModeDefault     Mode = "default"      // ask before mutations
	ModeAcceptEdits Mode = "accept-edits" // auto-approve edits, ask for bash
	ModePlan        Mode = "plan"         // read-only
	ModeAuto        Mode = "auto"         // approve by rule; hard blocks stand
	ModeBypass      Mode = "bypass"       // dangerous; refusable by org policy
)

type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

type Result struct {
	Decision Decision
	// Reason is shown to the user in the approval prompt and recorded in the
	// audit log, so it must name the rule that fired.
	Reason string
	// Scope is the suggested "always allow" rule, e.g. `bash(git commit *)`.
	Scope string
	// Step is which stage of the evaluation order decided: hook, deny,
	// destructive, screen, ask, mode, allow or default. The reason is prose
	// for a person; this is what lets a reviewer count how often each stage
	// is doing the work.
	Step string
	// Rule is the rule that decided, as written, for the deny, ask and allow
	// steps; "" when no rule did.
	Rule string
}

// Offer is the scope a person may "always allow", and the only one a
// remembered choice may satisfy: none unless the call asks by default.
func (r Result) Offer() string {
	if r.Decision != Ask || r.Step != "default" {
		return ""
	}
	return r.Scope
}

// Rule matches a tool call. Patterns are `tool` or `tool(arg-glob)`.
type Rule struct {
	raw     string
	tool    string
	pattern *regexp.Regexp // nil means "any argument"
	glob    string
	// folded is pattern with the literal start of its program name lowered, for
	// systems that ignore case; nil when that changes nothing.
	folded *regexp.Regexp
	// nfc is pattern in Unicode NFC, which deny and ask path rules also match
	// against NFC subjects; nil for a pattern with no non-ASCII character.
	nfc *regexp.Regexp
	// session marks a rule a person added for this session; see Overlay.
	session bool
}

func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, fmt.Errorf("empty rule")
	}
	r := Rule{raw: s, tool: s}

	// An unbalanced parenthesis used to fall through and become a tool named
	// "bash(", which matches nothing. For a deny rule that is worse than an
	// error: the operator writes it, sees no complaint, and believes they are
	// protected by a rule that can never fire.
	open := strings.Index(s, "(")
	if open >= 0 && !strings.HasSuffix(s, ")") {
		return Rule{}, fmt.Errorf("rule %q: missing the closing parenthesis "+
			"(a rule is a tool name, optionally followed by a pattern in "+
			"parentheses, e.g. bash(go test*))", s)
	}
	if open == 0 {
		return Rule{}, fmt.Errorf("rule %q: no tool name before the pattern", s)
	}
	if strings.HasSuffix(s, ")") && open < 0 {
		return Rule{}, fmt.Errorf("rule %q: closing parenthesis with no opening one", s)
	}

	if i := open; i > 0 && strings.HasSuffix(s, ")") {
		r.tool = s[:i]
		glob := s[i+1 : len(s)-1]
		re, err := globToRegexp(glob)
		if err != nil {
			return Rule{}, fmt.Errorf("rule %q: %w", s, err)
		}
		r.pattern, r.glob = re, glob
		if hasNonASCII(glob) {
			if r.nfc, err = globToRegexp(norm.NFC.String(glob)); err != nil {
				return Rule{}, fmt.Errorf("rule %q: %w", s, err)
			}
		}
		if lowered := lowerLiteralPrefix(glob); lowered != glob {
			if r.folded, err = globToRegexp(lowered); err != nil {
				return Rule{}, fmt.Errorf("rule %q: %w", s, err)
			}
		}
	}
	return r, nil
}

func globToRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	// (?s): a * spans newlines, or a newline anywhere would slip past a deny rule.
	b.WriteString("(?s)^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func (r Rule) Matches(tool string, subject string) bool {
	if r.tool != tool && r.tool != "*" {
		return false
	}
	if r.pattern == nil {
		return true
	}
	if r.pattern.MatchString(subject) {
		return true
	}
	// Folding only adds a match: the program name is lowered on both sides.
	if tool == "bash" && tools.FoldsCommandNames() {
		folded := r.pattern
		if r.folded != nil {
			folded = r.folded
		}
		if lowered := foldName(subject); lowered != subject || r.folded != nil {
			return folded.MatchString(lowered)
		}
	}
	return false
}

// lowerLiteralPrefix lowers a glob's start up to its first wildcard or space,
// the part that can only be a program name.
func lowerLiteralPrefix(glob string) string {
	end := strings.IndexAny(glob, "*? \t")
	if end < 0 {
		end = len(glob)
	}
	return strings.ToLower(glob[:end]) + glob[end:]
}

// foldName lowers a command's first word, its program name, where case is ignored.
func foldName(command string) string {
	start := len(command) - len(strings.TrimLeft(command, " \t"))
	end := strings.IndexAny(command[start:], " \t\n")
	if end < 0 {
		end = len(command) - start
	}
	return command[:start] + tools.CommandName(command[start:start+end]) + command[start+end:]
}

// rulesSeeParts reports whether a deny or ask rule with a pattern applies to tool.
func (e *Engine) rulesSeeParts(tool string) bool {
	deny, ask, _ := e.rules()
	return patterned(deny, tool) || patterned(ask, tool)
}

// patterned reports whether any of rules has a pattern for tool.
func patterned(rules []Rule, tool string) bool {
	for _, r := range rules {
		if (r.tool == tool || r.tool == "*") && r.pattern != nil {
			return true
		}
	}
	return false
}

// matchesEverything reports whether the rule names a tool with no narrower pattern.
func (r Rule) matchesEverything() bool { return r.pattern == nil || r.glob == "*" }

// NeverAllows reports whether an allow rule can never match: its bash pattern
// holds shell control syntax, which no command an allow rule approves may have.
func NeverAllows(rule string) bool {
	r, err := ParseRule(rule)
	return err == nil && r.tool == "bash" && hasShellControl(r.glob)
}

// matchesAny reports whether the rule matches any of the subjects.
func (r Rule) matchesAny(tool string, subjects []string) bool {
	for _, s := range subjects {
		if r.Matches(tool, s) {
			return true
		}
	}
	return false
}

// matchesPathAny is matchesAny for a deny or ask rule on a path, also in NFC on
// both sides: a rule pasted in NFD holds for the NFC name. It only adds matches.
func (r Rule) matchesPathAny(tool string, subjects []string) bool {
	if r.matchesAny(tool, subjects) {
		return true
	}
	if r.tool != tool && r.tool != "*" {
		return false
	}
	for _, s := range subjects {
		if !hasNonASCII(s) && r.nfc == nil {
			continue
		}
		re := r.pattern
		if r.nfc != nil {
			re = r.nfc
		}
		if re.MatchString(norm.NFC.String(s)) {
			return true
		}
	}
	return false
}

func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return true
		}
	}
	return false
}

func (r Rule) String() string { return r.raw }

// Tool is the tool name the rule is for, "*" for any.
func (r Rule) Tool() string { return r.tool }

// named is how a reason names the rule: a session rule says so.
func (r Rule) named() string {
	if r.session {
		return "session rule " + r.raw
	}
	return "rule " + r.raw
}

// Hook runs before rule evaluation. A Deny from it is final; an Ask applies
// after the deny, plan-mode and ask steps, before the mode; an Allow, or nil, is
// no opinion. A hook can tighten a decision and never loosen one.
type Hook func(tool string, args json.RawMessage) *Result

type Engine struct {
	Mode  Mode
	Deny  []Rule
	Ask   []Rule
	Allow []Rule
	Hooks []Hook
	// EngineHooks are hooks made for the engine evaluating, so one copied for
	// a subagent, with more roots, judges with those.
	EngineHooks []func(*Engine) Hook

	// Managed marks the engine as org-controlled: bypass mode is refused and
	// local config cannot escalate past it (docs P7, §10 precedence).
	Managed bool

	// Session holds the rules a person added for this session, evaluated
	// after the configured ones in each list. Nil adds none.
	Session *Overlay

	// Roots returns the workspace and added directories, so a path rule
	// written relative to one matches. Nil matches paths only as given.
	Roots func() []string

	// AskReadOnly names read-only tools that may still ask in the default,
	// accept-edits, auto and plan modes. Each is given the call's subject and
	// returns why it asks, or "" when it need not. An allow rule approves
	// them and bypass mode runs them. It is for a tool whose reads can carry
	// data out, such as web_fetch.
	AskReadOnly map[string]func(subject string) string

	// GitExtensions are git subcommands outside git itself, such as lfs, the
	// person opted in to (permissions.git_extensions): the destructive step
	// does not take them as an alias or extension it cannot read. Deny and
	// ask rules still hold for them.
	GitExtensions map[string]bool
}

func New(mode Mode) *Engine { return &Engine{Mode: mode} }

// AllowGitExtensions opts the named git extensions in; a name that is not a
// plain subcommand, or is one of git's own, is refused.
func (e *Engine) AllowGitExtensions(names ...string) error {
	for _, name := range names {
		if err := tools.GitExtensionError(name); err != nil {
			return err
		}
		if e.GitExtensions == nil {
			e.GitExtensions = map[string]bool{}
		}
		e.GitExtensions[name] = true
	}
	return nil
}

func (e *Engine) AddDeny(patterns ...string) error  { return addAll(&e.Deny, patterns) }
func (e *Engine) AddAsk(patterns ...string) error   { return addAll(&e.Ask, patterns) }
func (e *Engine) AddAllow(patterns ...string) error { return addAll(&e.Allow, patterns) }

func addAll(dst *[]Rule, patterns []string) error {
	for _, p := range patterns {
		r, err := ParseRule(p)
		if err != nil {
			return err
		}
		*dst = append(*dst, r)
	}
	return nil
}

// Screens reports whether a hook or a deny rule could refuse some calls to
// tool and not others. Something that cannot show each call to the engine,
// such as an interactive shell, cannot honour such a rule and must not run.
func (e *Engine) Screens(tool string) bool {
	if len(e.Hooks)+len(e.EngineHooks) > 0 {
		return true
	}
	for _, r := range e.DenyRules() {
		if r.tool == tool || r.tool == "*" {
			return true
		}
	}
	return false
}

// Subject extracts the string a rule matches against: the command for bash,
// the path for file tools, and — for the higher-privilege tools whose
// security-relevant argument is named differently — the verb or target they
// act on. This is what makes scoping per-command rather than per-tool.
//
// The extra keys are not cosmetic. With only command/path/pattern, a tool like
// k8s_get (whose target is `resource`) or k8s_apply (whose verb is `action`)
// produced an empty subject, so an argument-scoped rule against it could never
// match: an operator's `deny k8s_get(secrets*)` compiled and then silently
// never fired — the same "a rule that can never match" failure ParseRule
// refuses for bash. The list is priority order, most security-relevant first;
// the first key present wins, so a single subject string is returned as before.
//
// ssh takes both `command` and `host`; command wins here because it is the more
// consequential field and ssh already asks unconditionally. Scoping ssh by host
// needs a per-tool subject (a tool-declared Subjector), which is left as follow-up.
func Subject(tool string, args json.RawMessage) string {
	_, s, _ := subjectOf(args)
	if target, ok := clusterSubject(tool, args); ok {
		return target
	}
	return s
}

// clusterSubject is the subject of a Kubernetes tool, which names where the
// call goes first: the declared cluster, or `context:NAME` for a kubeconfig
// context (`context:` for the current one). k8s_login's subject is the
// cluster; k8s_get's is cluster/namespace/resource and k8s_apply's
// cluster/namespace/action, with an empty namespace for the call's default.
// So a rule or an "always allow" on one cluster never covers another.
func clusterSubject(tool string, args json.RawMessage) (string, bool) {
	if tool != "k8s_login" && tool != "k8s_get" && tool != "k8s_apply" {
		return "", false
	}
	m, err := tools.DecodeArgs(args)
	if err != nil {
		return "", false
	}
	str := func(key string) string {
		v, _ := tools.Lookup(m, key)
		s, _ := v.(string)
		return s
	}
	where := strings.TrimSpace(str("cluster"))
	if where == "" {
		where = "context:" + str("context")
	}
	// A cluster-scoped object, or a kind whose scope is not known, is judged
	// under a namespace no rule written for a real one can match.
	ns := str("namespace")
	resource := kubescope.Resource(str("resource"))
	switch tool {
	case "k8s_get":
		if kubescope.ClusterScopedResource(resource) {
			ns = kubescope.ClusterWide
		}
		return where + "/" + ns + "/" + resource, true
	case "k8s_apply":
		if str("action") == "apply" {
			// The manifest is read as the tool reads it; one that does not
			// decode strictly is judged cluster-wide.
			kind := ""
			if mf, err := kubescope.DecodeManifest(str("manifest")); err == nil {
				kind = mf.Kind
			}
			if namespaced, known := kubescope.KindScope(kind); !namespaced || !known {
				ns = kubescope.ClusterWide
			}
		} else if kubescope.ClusterScopedResource(resource) {
			ns = kubescope.ClusterWide
		}
		return where + "/" + ns + "/" + str("action"), true
	}
	return where, true
}

// subjectOf is Subject with the argument it came from. Arguments are decoded
// strictly and keys matched as a tool's struct matches them, so both read one value.
func subjectOf(args json.RawMessage) (key, subject string, err error) {
	m, err := tools.DecodeArgs(args)
	if err != nil {
		return "", "", err
	}
	for _, key := range tools.SubjectKeys {
		if v, found := tools.Lookup(m, key); found {
			if s, isStr := v.(string); isStr {
				return key, s, nil
			}
		}
	}
	return "", "", nil
}

// pathSubjects are the spellings a path rule is matched against. Deny and ask
// rules see every one: the path as given, absolute, with its links resolved,
// and relative to each root, with and without a leading ./. Allow rules see
// only the target: the resolved path, absolute and relative to the workspace.
func (e *Engine) pathSubjects(p string) (all, allow []string) {
	add := func(to *[]string, s string) {
		for _, have := range *to {
			if have == s {
				return
			}
		}
		*to = append(*to, s)
	}
	all = []string{p}
	if p == "" {
		return all, all
	}
	var roots []string
	if e.Roots != nil {
		for _, r := range e.Roots() {
			roots = append(roots, r)
			real := tools.RealPath(r)
			if real != r {
				roots = append(roots, real)
			}
			// A root named in another case than the disk holds would miss the disk's spelling below.
			if disk := tools.DiskPath(real); disk != real {
				roots = append(roots, disk)
			}
		}
	}
	// A folder given with a trailing separator names what is inside it, as write(**/locked/**) does.
	dir := ""
	if strings.HasSuffix(p, "/") || strings.HasSuffix(p, string(filepath.Separator)) {
		dir = "/"
	}
	abs := filepath.Clean(p)
	if !filepath.IsAbs(abs) {
		add(&all, filepath.ToSlash(abs)+dir)
		if len(roots) == 0 {
			return all, []string{filepath.ToSlash(abs) + dir}
		}
		abs = filepath.Join(roots[0], abs)
	}
	real := tools.RealPath(abs)
	relTo := func(to *[]string, root, a string) {
		if rel, err := filepath.Rel(root, a); err == nil && filepath.IsLocal(rel) {
			rel = filepath.ToSlash(rel) + dir
			add(to, rel)
			add(to, "./"+rel)
		}
	}
	// Deny and ask rules also see the case the disk holds: on a disk that folds case,
	// core/VAULT is core/vault. Allow rules keep the spelling given, so this only tightens.
	diskAbs, diskReal := tools.DiskPath(abs), tools.DiskPath(real)
	if real == abs {
		diskReal = diskAbs
	}
	for _, a := range []string{abs, real, diskAbs, diskReal} {
		add(&all, filepath.ToSlash(a)+dir)
		for _, root := range roots {
			relTo(&all, root, a)
		}
	}
	allow = []string{filepath.ToSlash(real) + dir}
	if len(roots) > 0 {
		relTo(&allow, tools.RealPath(roots[0]), real)
	}
	return all, allow
}

// pathRules reports whether any deny, ask or allow rule for tool has a pattern.
func (e *Engine) pathRules(tool string) bool {
	deny, ask, allow := e.rules()
	for _, rules := range [][]Rule{deny, ask, allow} {
		for _, r := range rules {
			if (r.tool == tool || r.tool == "*") && r.pattern != nil {
				return true
			}
		}
	}
	return false
}

// Evaluate applies the ordered decision flow. A prompt shows the command as
// written; its reason notes continuations the checks joined.
func (e *Engine) Evaluate(tool string, mutates bool, args json.RawMessage) Result {
	return e.evaluateAs(tool, mutates, args, false)
}

// EvaluateLine judges one line typed at an interactive terminal. The shell
// may hold lines before it and finish it with lines after, so a line a bash
// parser cannot read on its own (for f in *; do, done, an open quote) is
// judged by the text checks alone, not refused for that; every other step
// is Evaluate's.
func (e *Engine) EvaluateLine(tool string, mutates bool, args json.RawMessage) Result {
	return e.evaluateAs(tool, mutates, args, true)
}

func (e *Engine) evaluateAs(tool string, mutates bool, args json.RawMessage, line bool) Result {
	res := e.evaluate(tool, mutates, args, line)
	if res.Decision == Ask && tool == "bash" {
		// Only a backslash before a newline can be joined; the rest is not parsed again.
		if _, subject, err := subjectOf(args); err == nil && strings.Contains(subject, "\\\n") && tools.CanonicalCommand(subject).Joined {
			res.Reason += " (the command continues lines with backslash-newline; it was checked joined)"
		}
	}
	return res
}

func (e *Engine) evaluate(tool string, mutates bool, args json.RawMessage, line bool) Result {
	key, subject, err := subjectOf(args)
	if err != nil {
		return Result{Decision: Deny, Reason: err.Error(), Scope: "", Step: "args"}
	}
	// Deny and ask rules see each command in a bash chain. A narrow allow rule
	// approves only a simple command, and never a multi-line subject.
	subjects, narrowAllows, complete := []string{subject}, !strings.ContainsAny(subject, "\n\r"), true
	allowSubjects := []string{subject}
	var canon tools.Canonical
	switch {
	case tool == "bash":
		// Every step sees the command as written and as the shell splits it,
		// continuations joined; an allow rule sees no more than the joined form.
		canon = tools.CanonicalCommand(subject)
		if line {
			canon = canon.AsTerminalLine()
		}
		subjects, complete = commandSegments(subject)
		narrowAllows = !hasShellControl(subject)
		if canon.Text != subject {
			more, whole := commandSegments(canon.Text)
			subjects, complete = append(subjects, more...), complete && whole
		}
		// Under a changed IFS, deny and ask rules also read the words it splits.
		if canon.IFS != "" {
			more, whole := commandSegments(tools.IFSSplitText(canon.Text, canon.IFS))
			subjects, complete = append(subjects, more...), complete && whole
		}
		// And the commands a bash parser finds, in function bodies and in the
		// text eval, trap and -c run, read whole: their quoting is undone.
		subjects = append(subjects, canon.Commands()...)
		if canon.Joined && !canon.Reworded {
			allowSubjects, narrowAllows = []string{canon.Text}, !hasShellControl(canon.Text)
		}
		if canon.Reworded {
			narrowAllows = false
		}
	case key == "path" && e.pathRules(tool):
		subjects, allowSubjects = e.pathSubjects(subject)
	}
	// A Kubernetes call is judged on where it goes. Deny and ask rules also
	// see the argument its subject used to be, so a rule written on a
	// resource, a verb or a namespace still holds; allow rules and the
	// offered scope see only the cluster-first subject.
	if target, ok := clusterSubject(tool, args); ok {
		subjects, allowSubjects = []string{target}, []string{target}
		if subject != "" && subject != target {
			subjects = append(subjects, subject)
		}
		subject, narrowAllows = target, !strings.ContainsAny(target, "\n\r")
	}

	// 1. Hooks — arbitrary operator logic, evaluated first so it can veto. A
	// hook's refusal is final; its ask waits for the deny, plan-mode and ask
	// steps below, so a hook can never turn a refusal into a question.
	var hookAsk *Result
	hooks := e.Hooks
	for _, bind := range e.EngineHooks {
		hooks = append(hooks[:len(hooks):len(hooks)], bind(e))
	}
	for _, h := range hooks {
		res := h(tool, args)
		if res == nil || res.Decision == Allow {
			continue
		}
		if res.Step == "" {
			res.Step = "hook"
		}
		if res.Decision != Ask {
			return *res
		}
		if hookAsk == nil {
			hookAsk = res
		}
	}

	// Deny and ask rules on a path also match in NFC; allow rules match only as written.
	matches := Rule.matchesAny
	if key == "path" && tool != "bash" {
		matches = Rule.matchesPathAny
	}
	// A Kubernetes call on every namespace, `*`, reads each one: a deny or ask
	// rule matches it when the rule would match some namespace it covers.
	if _, k8s := clusterSubject(tool, args); k8s && strings.ContainsAny(subject, "*?") {
		matches = func(r Rule, tool string, subjects []string) bool {
			if r.matchesAny(tool, subjects) {
				return true
			}
			return (r.tool == tool || r.tool == "*") && r.pattern != nil && globsMeet(r.glob, subject)
		}
	}

	// 2. Deny rules — absolute, survive every mode including bypass.
	denyRules, askRules, allowRules := e.rules()
	for _, r := range denyRules {
		if matches(r, tool, subjects) {
			return Result{Decision: Deny, Reason: "denied by " + r.named(), Scope: "", Step: "deny", Rule: r.raw}
		}
	}
	// A program named by an expansion with an operator, ${x%/} or ${x//_/ },
	// is edited at run time where no deny rule can read it, so it is refused.
	if canon.ProgramHidden() && patterned(denyRules, tool) {
		return Result{Decision: Deny, Reason: "the program is named by an expansion that edits its value, so it cannot be checked against the deny rules", Scope: "", Step: "screen"}
	}
	if why := canon.Opaque(); why != "" && patterned(denyRules, tool) {
		return Result{Decision: Deny, Reason: "the command runs text no rule can read (" + why + "), so it cannot be checked against the deny rules", Scope: "", Step: "screen"}
	}
	if canon.Incomplete() && patterned(denyRules, tool) {
		return Result{Decision: Deny, Reason: "the command is not complete (an open quote, block or heredoc, or a trailing | or &&), so it cannot be checked against the deny rules", Scope: "", Step: "screen"}
	}
	if canon.Unparsed() && patterned(denyRules, tool) {
		return Result{Decision: Deny, Reason: "the command cannot be parsed as the shell reads it, so it cannot be checked against the deny rules", Scope: "", Step: "screen"}
	}

	// 2a. Under a changed IFS an expansion's value splits into words no rule
	// can read; a deny rule must hold in every mode, so this refuses, not asks.
	if canon.IFSSplit && patterned(denyRules, tool) {
		return Result{Decision: Deny, Reason: "the command changes IFS and then expands words, so they cannot be checked against the deny rules", Scope: "", Step: "screen"}
	}

	// 2b. Plan mode changes nothing, so a mutating call is refused before
	// anything could put it to a person who might accept it.
	if e.Mode == ModePlan && mutates {
		return Result{Decision: Deny, Reason: "plan mode is read-only; no changes are applied", Scope: "", Step: "mode"}
	}

	// 2c. Destructive commands always confirm, in every mode. There is no
	// undo for these, so no mode auto-approves them (docs P7, §06).
	if tool == "bash" {
		if what, destructive := canon.DestructiveWith(e.GitExtensions); destructive {
			return Result{Decision: Ask, Reason: fmt.Sprintf("%s — always requires confirmation", what), Scope: "", Step: "destructive"}
		}
	}

	// 2d. A command too long or tangled to split in full may hide a part a
	// deny or ask rule would match, so no mode or allow rule approves it.
	if !complete && e.rulesSeeParts(tool) {
		return Result{Decision: Ask, Reason: "the command is too long or complex to check each part against the rules", Scope: "", Step: "screen"}
	}

	// 3. Ask rules — force a prompt even if a later allow would match, so they
	// offer no scope: a remembered one would stop them asking.
	for _, r := range askRules {
		if matches(r, tool, subjects) {
			return Result{Decision: Ask, Reason: "matched ask " + r.named(), Scope: "", Step: "ask", Rule: r.raw}
		}
	}

	// Words split or glued by an expansion may hide a part a rule would match.
	if canon.Hidden && e.rulesSeeParts(tool) {
		return Result{Decision: Ask, Reason: "the command builds its words with an expansion, brace list or IFS, so its parts cannot be checked against the rules", Scope: "", Step: "screen"}
	}

	// A hook's ask comes after the destructive, screen and ask-rule prompts, so
	// the record names the stronger reason; before the mode, so no mode skips it.
	if hookAsk != nil {
		return *hookAsk
	}

	// 4. Permission mode.
	switch e.Mode {
	case ModePlan:
		if mutates {
			return Result{Decision: Deny, Reason: "plan mode is read-only; no changes are applied", Scope: "", Step: "mode"}
		}
		// A read that can carry data out is not made safe by plan mode, which
		// any client may narrow a session to: it goes on to the allow rules and asks.
		if e.readOnlyAsk(tool, subject) == "" {
			return Result{Decision: Allow, Reason: readOnlyReason(tool) + " in plan mode", Scope: "", Step: "mode"}
		}
	case ModeBypass:
		if e.Managed {
			return Result{Decision: Ask, Reason: "bypass mode is disabled by organization policy", Scope: "", Step: "mode"}
		}
		return Result{Decision: Allow, Reason: "bypass mode", Scope: "", Step: "mode"}
	case ModeAcceptEdits:
		if tool == "edit" || tool == "write" {
			return Result{Decision: Allow, Reason: "edits auto-approved in accept-edits mode", Scope: "", Step: "mode"}
		}
	case ModeAuto:
		if !mutates && e.readOnlyAsk(tool, subject) == "" {
			return Result{Decision: Allow, Reason: readOnlyReason(tool) + " in auto mode", Scope: "", Step: "mode"}
		}
		// Auto mode approves in-workspace file mutations; the destructive-command
		// and deny checks above still stand, so the dangerous cases never reach
		// here. bash keeps asking because its blast radius is unbounded.
		if tool == "edit" || tool == "write" {
			return Result{Decision: Allow, Reason: "file edit auto-approved in auto mode", Scope: "", Step: "mode"}
		}
	}

	// 5. Allow rules.
	for _, r := range allowRules {
		if (narrowAllows || r.matchesEverything()) && r.matchesAny(tool, allowSubjects) {
			return Result{Decision: Allow, Reason: "matched allow " + r.named(), Scope: "", Step: "allow", Rule: r.raw}
		}
	}

	// 6. Default: read-only tools proceed, mutations ask.
	if why := e.readOnlyAsk(tool, subject); why != "" && !mutates {
		return Result{Decision: Ask, Reason: why, Scope: suggestScope(tool, subject), Step: "default"}
	}
	if !mutates {
		return Result{Decision: Allow, Reason: readOnlyReason(tool), Scope: "", Step: "default"}
	}
	return Result{Decision: Ask, Reason: askReason(tool, e.Mode), Scope: suggestScope(tool, subject), Step: "default"}
}

// sessionControl names tools that change only the session's own background
// work: they need no approval, but they are not reads.
var sessionControl = map[string]bool{"shell_kill": true, "task_cancel": true}

// readOnlyReason is why a call that changes nothing outside the session is allowed.
func readOnlyReason(tool string) string {
	switch {
	case sessionControl[tool]:
		return "session control tool (stops this session's own background work)"
	case tool == "task":
		// It starts work, maybe in the background: "read-only" misdescribed it.
		return "subagent tool (each call the subagent makes is decided on its own)"
	case tool == "shell_output" || tool == "task_status":
		return "session tool (reads this session's own background work)"
	}
	return "read-only tool"
}

// readOnlyAsk is why a read-only call asks anyway, or "".
func (e *Engine) readOnlyAsk(tool, subject string) string {
	if f := e.AskReadOnly[tool]; f != nil {
		return f(subject)
	}
	return ""
}

// askReason says why a call is put to a person. A command is asked about
// because it can do anything, not because it is known to change something.
func askReason(tool string, mode Mode) string {
	if mode == "" {
		mode = ModeDefault
	}
	what := tool + " can make changes, so it"
	switch tool {
	case "bash":
		what = "running a command"
	case "edit", "write":
		what = "changing a file"
	}
	return fmt.Sprintf("%s needs approval in %s mode", what, mode)
}

// suggestScope proposes a narrow "always allow" rule for the approval prompt.
// Narrow by construction: allowing `git commit *` must never allow `rm`.
func suggestScope(tool, subject string) string {
	if subject == "" {
		return tool
	}
	if tool == "bash" {
		// No rule approves a chain, and a remembered scope is looked up by
		// name, so `git status && curl x | sh` must not suggest `git status *`.
		if hasShellControl(subject) {
			return ""
		}
		fields := strings.Fields(subject)
		if len(fields) == 0 {
			return tool
		}
		fields[0] = tools.CommandName(fields[0])
		prefix := bashScope(fields)
		if prefix == "" {
			return ""
		}
		return fmt.Sprintf("%s(%s *)", tool, prefix)
	}
	// A page's site, not the page: "always allow" for one URL would ask again
	// for the next page there.
	if tool == "web_fetch" {
		if u, err := url.Parse(subject); err == nil && u.Scheme != "" && u.Host != "" {
			return fmt.Sprintf("%s(%s://%s/*)", tool, u.Scheme, u.Host)
		}
	}
	return fmt.Sprintf("%s(%s)", tool, subject)
}

// globsMeet reports whether some string matches both glob patterns, where
// `*` is any run of characters and `?` any one.
func globsMeet(a, b string) bool {
	type pos struct{ i, j int }
	seen := map[pos]bool{}
	var meet func(i, j int) bool
	meet = func(i, j int) bool {
		if i == len(a) && j == len(b) {
			return true
		}
		p := pos{i, j}
		if done, ok := seen[p]; ok {
			return done
		}
		seen[p] = false
		ok := false
		switch {
		case i < len(a) && a[i] == '*':
			ok = meet(i+1, j) || (j < len(b) && meet(i, j+1))
		case j < len(b) && b[j] == '*':
			ok = meet(i, j+1) || (i < len(a) && meet(i+1, j))
		case i < len(a) && j < len(b) && (a[i] == b[j] || a[i] == '?' || b[j] == '?'):
			ok = meet(i+1, j+1)
		}
		seen[p] = ok
		return ok
	}
	return meet(0, 0)
}
