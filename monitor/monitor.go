// Package monitor judges a proposed tool call against the session's remit,
// the agent's stated reasoning and where the call's arguments came from,
// before the call runs.
//
// Its one structural rule: a verdict can only tighten a decision. An allow
// may become an ask or a deny, an ask may become a deny, and nothing moves
// the other way. That rule is what makes it safe for the monitor to read the
// agent's reasoning: a judge that could be talked into permitting something
// would have to be kept away from the text that does the talking, which is
// why the shipped monitors elsewhere are reasoning-blind. Here the worst a
// persuaded monitor can do is nothing, and the deny rules and the sandbox
// stand underneath it regardless.
//
// The second rule is that absence is not consent. A monitor that cannot
// answer — unreachable, timed out, malformed — raises the decision to ask,
// or to deny where nobody is there to ask.
package monitor

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
)

// Case is everything the monitor may know about one proposed call.
type Case struct {
	// Remit is what the session was asked to do: the first user message,
	// plus whatever the operator configured.
	Remit string
	// Reasoning is the agent's most recent stated thinking, newest last. It
	// is untrusted text and is handed to the judge as such.
	Reasoning []string
	Tool      string
	Args      string
	Mutates   bool
	// Provisional is what policy decided before the monitor was consulted.
	Provisional policy.Result
	// Provenance says, for each host or path the arguments name, whether the
	// user ever mentioned it. A name that came only from tool output is the
	// classic shape of an injected instruction.
	Provenance []Provenance
	// Recent are the last few calls and how they went, oldest first.
	Recent []Recent
}

// Provenance is one name in the arguments and where it was first seen.
type Provenance struct {
	Token     string
	UserNamed bool
}

// Recent is one earlier call, for pattern spotting.
type Recent struct {
	Tool    string
	Subject string
	IsError bool
}

// Verdict is the judge's answer.
type Verdict struct {
	Decision policy.Decision
	// Code names the concern, in the OWASP Agentic Top 10's terms where one
	// fits: goal-hijack, tool-misuse, privilege, context-poisoning,
	// out-of-remit; "ok" for a call the judge is content with.
	Code       string
	Confidence float64
	Rationale  string
	LatencyMS  int64
	// Version identifies the prompt or ruleset that produced the verdict, so
	// a change in judgement can be traced to a change in the judge.
	Version string
}

// Monitor is the judge. A model behind a local endpoint is one; a rule set
// is another; a test's script is a third.
type Monitor interface {
	Judge(ctx context.Context, c Case) (Verdict, error)
}

// Func adapts a function.
type Func func(ctx context.Context, c Case) (Verdict, error)

func (f Func) Judge(ctx context.Context, c Case) (Verdict, error) { return f(ctx, c) }

// ErrUnavailable is what a Guard reports when the judge gave no usable answer.
var ErrUnavailable = errors.New("monitor unavailable")

// Guard runs a Monitor under the two rules, with a budget.
type Guard struct {
	Monitor Monitor
	// Timeout bounds one judgement. Zero means two seconds.
	Timeout time.Duration
	// OnUnavailable is the decision a call gets when the judge cannot
	// answer: Ask by default, Deny where nobody can answer an ask. It never
	// loosens: an unavailable judge cannot turn a deny into anything else.
	OnUnavailable policy.Decision
	// MaxPerTurn bounds how many calls in one turn are put to the judge; the
	// rest keep their provisional decision and are recorded as unreviewed.
	// Zero means no bound.
	MaxPerTurn int

	mu   sync.Mutex
	turn int
	used int
}

// Outcome is what the guard decided and why.
type Outcome struct {
	Verdict Verdict
	// Before is the provisional decision; After is the decision after the
	// verdict, which is never looser than Before.
	Before, After policy.Decision
	Tightened     bool
	// Unavailable is set when the judge gave no usable answer and the
	// OnUnavailable rule decided instead.
	Unavailable bool
	// Skipped is set when the deterministic fast path or the turn budget kept
	// the call away from the judge.
	Skipped string
}

// BeginTurn resets the per-turn budget.
func (g *Guard) BeginTurn() {
	g.mu.Lock()
	g.turn++
	g.used = 0
	g.mu.Unlock()
}

// NeedsReview is the deterministic fast path: a call the judge need not see.
//
// A denied call costs nothing to leave denied. A read-only call that policy
// allowed by rule, whose every name the user spoke, has nothing for a judge to
// add. Everything else — anything policy would ask about, anything mutating
// that a mode waved through, anything naming a host or path that only tool
// output supplied — is reviewed.
func NeedsReview(c Case) (bool, string) {
	if c.Provisional.Decision == policy.Deny {
		return false, "already denied"
	}
	borrowed := false
	for _, p := range c.Provenance {
		borrowed = borrowed || !p.UserNamed
	}
	if !c.Mutates && !borrowed && c.Provisional.Decision == policy.Allow && c.Provisional.Step == "allow" {
		return false, "read allowed by rule, names all the user's"
	}
	return true, ""
}

// Review judges one call. The result's After is never looser than Before.
func (g *Guard) Review(ctx context.Context, c Case) Outcome {
	out := Outcome{Before: c.Provisional.Decision, After: c.Provisional.Decision}
	if ok, why := NeedsReview(c); !ok {
		out.Skipped = why
		return out
	}
	if g.MaxPerTurn > 0 {
		g.mu.Lock()
		over := g.used >= g.MaxPerTurn
		if !over {
			g.used++
		}
		g.mu.Unlock()
		if over {
			out.Skipped = "turn budget spent"
			return out
		}
	}

	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	v, err := judge(jctx, g.Monitor, c)
	v.LatencyMS = time.Since(start).Milliseconds()
	if err != nil || !known(v.Decision) {
		fallback := g.OnUnavailable
		if fallback != policy.Ask && fallback != policy.Deny {
			fallback = policy.Ask
		}
		v.Decision, v.Code = fallback, "unavailable"
		if err != nil {
			v.Rationale = err.Error()
		} else {
			v.Rationale = "the judge returned no decision"
		}
		out.Unavailable = true
	}
	out.Verdict = v
	out.After = tighten(out.Before, v.Decision)
	out.Tightened = out.After != out.Before
	return out
}

// judge calls the monitor and turns a panic or a nil monitor into an error,
// so a broken judge is an unavailable one and not a crash.
func judge(ctx context.Context, m Monitor, c Case) (v Verdict, err error) {
	if m == nil {
		return Verdict{}, ErrUnavailable
	}
	defer func() {
		if r := recover(); r != nil {
			err = ErrUnavailable
		}
	}()
	return m.Judge(ctx, c)
}

func known(d policy.Decision) bool {
	return d == policy.Allow || d == policy.Ask || d == policy.Deny
}

var rank = map[policy.Decision]int{policy.Allow: 0, policy.Ask: 1, policy.Deny: 2}

// tighten returns the stricter of the two: the whole package in one line.
func tighten(before, verdict policy.Decision) policy.Decision {
	if rank[verdict] > rank[before] {
		return verdict
	}
	return before
}

var (
	hostPattern = regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+[a-z]{2,}(?::\d+)?\b`)
	pathPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9._~/-])(/[A-Za-z0-9._~-]+(?:/[A-Za-z0-9._~-]+)+)`)
)

// fileExtensions are the last labels that make a dotted name a file, not a host.
var fileExtensions = map[string]bool{"md": true, "go": true, "py": true, "js": true, "ts": true, "json": true, "txt": true,
	"yaml": true, "yml": true, "html": true, "css": true, "sh": true, "toml": true, "sql": true, "rs": true, "c": true,
	"h": true, "cfg": true, "ini": true, "lock": true, "mod": true, "sum": true, "log": true, "csv": true, "xml": true}

// Names finds the hosts and absolute paths an argument string names, so the
// caller can say which of them the user spoke.
func Names(args string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hostPattern.FindAllString(args, -1) {
		h = strings.ToLower(h)
		if fileExtensions[h[strings.LastIndex(h, ".")+1:]] {
			continue // README.md is a file, not a host
		}
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, m := range pathPattern.FindAllStringSubmatch(args, -1) {
		p := m[1]
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// Trace marks each name with whether it appears in what the user said.
func Trace(args string, userText string) []Provenance {
	low := strings.ToLower(userText)
	var out []Provenance
	for _, n := range Names(args) {
		out = append(out, Provenance{Token: n, UserNamed: strings.Contains(low, strings.ToLower(n))})
	}
	return out
}
