// Package policy implements Abhed's permission model.
//
// Evaluation is ordered (docs P7):
//
//	Hooks → Deny rules → Ask rules → Permission mode → Allow rules → Callback
//
// Deny is absolute: a matching deny rule blocks the tool even in the most
// permissive mode. Rules are scoped per-command, not per-tool, so allowing
// `bash(npm test)` never allows `bash(rm -rf /)`.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

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
	// Scope is the suggested "always allow" rule, e.g. `bash(npm install *)`.
	Scope string
	// Step is which stage of the evaluation order decided: hook, deny, ask,
	// mode, allow or default. The reason is prose for a person; this is what
	// lets a reviewer count how often each stage is doing the work.
	Step string
}

// Rule matches a tool call. Patterns are `tool` or `tool(arg-glob)`.
type Rule struct {
	raw     string
	tool    string
	pattern *regexp.Regexp // nil means "any argument"
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
		r.pattern = re
	}
	return r, nil
}

func globToRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
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
	return r.pattern.MatchString(subject)
}

func (r Rule) String() string { return r.raw }

// Hook runs before rule evaluation and can short-circuit the decision.
type Hook func(tool string, args json.RawMessage) *Result

type Engine struct {
	Mode  Mode
	Deny  []Rule
	Ask   []Rule
	Allow []Rule
	Hooks []Hook

	// Managed marks the engine as org-controlled: bypass mode is refused and
	// local config cannot escalate past it (docs P7, §10 precedence).
	Managed bool
}

func New(mode Mode) *Engine { return &Engine{Mode: mode} }

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
	if len(e.Hooks) > 0 {
		return true
	}
	for _, r := range e.Deny {
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
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	for _, key := range []string{"command", "path", "pattern", "action", "resource", "host", "namespace", "name"} {
		if v, found := m[key]; found {
			if s, isStr := v.(string); isStr {
				return s
			}
		}
	}
	return ""
}

// Evaluate applies the ordered decision flow.
func (e *Engine) Evaluate(tool string, mutates bool, args json.RawMessage) Result {
	subject := Subject(tool, args)

	// 1. Hooks — arbitrary operator logic, evaluated first so it can veto.
	for _, h := range e.Hooks {
		if res := h(tool, args); res != nil {
			if res.Step == "" {
				res.Step = "hook"
			}
			return *res
		}
	}

	// 2. Deny rules — absolute, survive every mode including bypass.
	for _, r := range e.Deny {
		if r.Matches(tool, subject) {
			return Result{Decision: Deny, Reason: fmt.Sprintf("denied by rule %s", r), Scope: "", Step: "deny"}
		}
	}

	// 2b. Destructive commands always confirm, in every mode. There is no
	// undo for these, so no mode auto-approves them (docs P7, §06).
	if tool == "bash" {
		if what, destructive := tools.IsDestructive(subject); destructive {
			return Result{Decision: Ask, Reason: fmt.Sprintf("%s — always requires confirmation", what), Scope: "", Step: "destructive"}
		}
	}

	// 3. Ask rules — force a prompt even if a later allow would match.
	for _, r := range e.Ask {
		if r.Matches(tool, subject) {
			return Result{Decision: Ask, Reason: fmt.Sprintf("matched ask rule %s", r), Scope: suggestScope(tool, subject), Step: "ask"}
		}
	}

	// 4. Permission mode.
	switch e.Mode {
	case ModePlan:
		if mutates {
			return Result{Decision: Deny, Reason: "plan mode is read-only; no changes are applied", Scope: "", Step: "mode"}
		}
		return Result{Decision: Allow, Reason: "read-only tool in plan mode", Scope: "", Step: "mode"}
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
		if !mutates {
			return Result{Decision: Allow, Reason: "read-only tool in auto mode", Scope: "", Step: "mode"}
		}
		// Auto mode approves in-workspace file mutations; the destructive-command
		// and deny checks above still stand, so the dangerous cases never reach
		// here. bash keeps asking because its blast radius is unbounded.
		if tool == "edit" || tool == "write" {
			return Result{Decision: Allow, Reason: "file edit auto-approved in auto mode", Scope: "", Step: "mode"}
		}
	}

	// 5. Allow rules.
	for _, r := range e.Allow {
		if r.Matches(tool, subject) {
			return Result{Decision: Allow, Reason: fmt.Sprintf("matched allow rule %s", r), Scope: "", Step: "allow"}
		}
	}

	// 6. Default: read-only tools proceed, mutations ask.
	if !mutates {
		return Result{Decision: Allow, Reason: "read-only tool", Scope: "", Step: "default"}
	}
	return Result{Decision: Ask, Reason: "mutating tool requires approval", Scope: suggestScope(tool, subject), Step: "default"}
}

// suggestScope proposes a narrow "always allow" rule for the approval prompt.
// Narrow by construction: allowing `npm install *` must never allow `rm`.
func suggestScope(tool, subject string) string {
	if subject == "" {
		return tool
	}
	if tool == "bash" {
		fields := strings.Fields(subject)
		if len(fields) == 0 {
			return tool
		}
		// Two tokens capture the meaningful verb ("npm install", "git status").
		prefix := fields[0]
		if len(fields) > 1 && !strings.HasPrefix(fields[1], "-") {
			prefix += " " + fields[1]
		}
		return fmt.Sprintf("%s(%s *)", tool, prefix)
	}
	return fmt.Sprintf("%s(%s)", tool, subject)
}
