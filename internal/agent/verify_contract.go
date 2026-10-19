package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Contract is an operator's completion contract: what must hold before a run
// may end completed. It comes from configuration the agent cannot reach (the
// .abhed directory and the files above it), never from the conversation.
type Contract struct {
	// Require are commands the harness runs, in the session's sandbox, once
	// the agent's last change is made. Each must exit 0.
	Require []string
	// Protect are globs of files the run must not change, such as tests and
	// CI. A pattern without a slash matches a name at any depth.
	Protect []string
	// TimeoutMS bounds each required command; zero is the bash tool's default.
	TimeoutMS int
}

// ContractResult is what the contract found, in answer.verdict.
type ContractResult struct {
	OK      bool            `json:"ok"`
	Require []RequireResult `json:"require,omitempty"`
	// Protected names the protected files the run changed, added or removed.
	Protected []string `json:"protected_changed,omitempty"`
	// ProtectPartial marks protect globs matching more files than are
	// hashed, so the contract could not check them all and is not met.
	ProtectPartial bool `json:"protect_partial,omitempty"`
}

// RequireResult is one required command's outcome.
type RequireResult struct {
	Command string `json:"command"`
	Seq     int64  `json:"seq,omitempty"` // its observation
	Exit    *int   `json:"exit_code,omitempty"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	// Reused marks a result taken from an earlier run of the same command
	// that no change has followed.
	Reused bool `json:"reused,omitempty"`
}

// ContractVia marks the calls the harness makes for the contract.
const ContractVia = "completion contract"

// ByOperator marks a call the operator's own configuration approved, as a
// completion contract's required command.
const ByOperator = "operator"

// runContract runs what the contract requires and compares the protected
// files with the run's start.
func (l *Loop) runContract(ctx context.Context) *ContractResult {
	c := l.Verify.Contract
	res := &ContractResult{OK: true}
	for _, cmd := range c.Require {
		r := l.requireOne(ctx, cmd, c.TimeoutMS)
		res.Require = append(res.Require, r)
		res.OK = res.OK && r.OK
	}
	if len(c.Protect) > 0 {
		l.Verify.mu.Lock()
		before := l.Verify.protect
		l.Verify.mu.Unlock()
		after, partial := c.snapshot(l.Session)
		l.Verify.mu.Lock()
		partial = partial || l.Verify.protectPartial
		l.Verify.mu.Unlock()
		res.Protected = changedFiles(before, after)
		// Past the bound the rest went unhashed, so a change there would go
		// unseen: the contract cannot say the files were left alone.
		res.ProtectPartial = partial
		if len(res.Protected) > 0 || partial {
			res.OK = false
		}
	}
	return res
}

// requireOne runs one required command, unless an earlier run of it passed
// and nothing has changed since. Policy still judges it, and anything it
// will not let run unasked fails the contract, closed.
func (l *Loop) requireOne(ctx context.Context, cmd string, timeoutMS int) RequireResult {
	r := RequireResult{Command: cmd}
	if events, err := l.Recorder.store.Events(l.Recorder.sessionID); err == nil {
		ix := indexRecord(checkEnv{events: events, mutates: l.recordedMutates})
		last := ix.lastChange()
		for i := len(ix.obs) - 1; i >= 0; i-- {
			o := ix.obs[i]
			if o.via == ContractVia && o.command == cmd && (last == nil || o.pos > last.pos) {
				if o.exit != nil && *o.exit == 0 && !o.isError {
					return RequireResult{Command: cmd, Seq: o.seq, Exit: o.exit, OK: true, Reused: true}
				}
				break
			}
		}
	}
	bash, ok := l.Tools.Get("bash")
	if !ok {
		r.Reason = "this session has no bash tool to run it with"
		return r
	}
	a := map[string]any{"command": cmd, "description": ContractVia}
	if timeoutMS > 0 {
		a["timeout_ms"] = timeoutMS
	}
	args, _ := json.Marshal(a)
	// The operator's declaration stands in for the approval an ordinary
	// default-mode ask wants, as an allow rule of theirs would. Any other
	// ask (a rule, a destructive command, a screen, a hook, or a managed
	// policy) still binds: the harness does not run it unasked.
	d := l.Policy.Evaluate("bash", tools.MutatesCall(bash, args), args)
	switch {
	case d.Decision == policy.Deny:
		r.Reason = "policy denies it: " + d.Reason
		return r
	case d.Decision == policy.Ask && (d.Step != "default" || l.Policy.Managed):
		r.Reason = fmt.Sprintf("policy asks before it runs (%s: %s); the harness does not run it unasked. "+
			"An allow rule such as bash(%s) lets it run", d.Step, d.Reason, cmd)
		return r
	}
	call := model.ToolCall{ID: "contract_" + newID(), Name: "bash", Args: args}
	if _, err := l.Recorder.Record(EvActionRequested, ActorSystem, Trusted, ActionRequested{
		CallID: call.ID, Tool: "bash", Args: args, Via: ContractVia,
		Reason: "required by the operator's completion contract",
	}); err != nil {
		r.Reason = "could not record it: " + err.Error()
		return r
	}
	// Every call that runs has its approval on record: here the operator's,
	// by the contract they declared.
	if _, err := l.Recorder.Record(EvActionApproved, ActorSystem, Trusted, withRule(map[string]string{
		"call_id": call.ID, "reason": "required by the operator's completion contract (" + d.Reason + ")",
		"step": d.Step, "by": ByOperator,
	}, d)); err != nil {
		r.Reason = "could not record its approval: " + err.Error()
		return r
	}
	res, _ := l.invoke(ctx, call)
	l.failuresMu.Lock()
	delete(l.repeatedFailures, call.Name+string(call.Args))
	l.failuresMu.Unlock()
	r.Exit = res.ExitCode
	r.Seq = l.Recorder.LastAppended()
	switch {
	case ctx.Err() != nil:
		r.Reason = "stopped before it finished"
	case res.ExitCode == nil:
		r.Reason = firstLine(clipRunes(res.Content, 200))
	case *res.ExitCode != 0:
		r.Reason = fmt.Sprintf("exited %d: %s", *res.ExitCode, tailLines(res.Content))
	default:
		r.OK = !res.IsError
		if !r.OK {
			r.Reason = tailLines(res.Content)
		}
	}
	return r
}

// maxProtected bounds the files hashed for protect, so a broad glob on a
// large tree cannot stall the end of every run. A var for the tests.
var maxProtected = 5000

// snapshot hashes the files the protect globs match under the session's root.
// partial is set when more files match than maxProtected allows.
func (c *Contract) snapshot(sess *tools.Session) (out map[string]string, partial bool) {
	out = map[string]string{}
	if sess == nil || sess.Root == "" {
		return out, false
	}
	var res []*regexp.Regexp
	for _, g := range c.Protect {
		if re := globRegexp(g); re != nil {
			res = append(res, re)
		}
	}
	root := sess.Root
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable folder is skipped, not fatal
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (name == ".git" || name == "node_modules" || name == tools.StateDir || name == tools.WorktreesDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil //nolint:nilerr // outside the root: not ours to judge
		}
		rel = filepath.ToSlash(rel)
		for _, re := range res {
			if re.MatchString(rel) {
				if len(out) >= maxProtected {
					partial = true
					return filepath.SkipAll
				}
				out[rel] = hashFile(p)
				break
			}
		}
		return nil
	})
	return out, partial
}

func hashFile(p string) string {
	data, err := os.ReadFile(p) //nolint:gosec // a path the walk found under the session's root
	if err != nil {
		return "unreadable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func changedFiles(before, after map[string]string) []string {
	var out []string
	for p, h := range after {
		if before[p] != h {
			out = append(out, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// globRegexp turns a protect glob into a match on a slash-separated path
// relative to the root: * stays within a folder, ** crosses them, and a
// pattern without a slash matches the name at any depth.
func globRegexp(g string) *regexp.Regexp {
	g = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(g)), "./")
	if g == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("^")
	if !strings.Contains(strings.TrimSuffix(g, "/"), "/") {
		b.WriteString("(?:.*/)?")
	}
	g = strings.TrimPrefix(g, "/")
	if strings.HasSuffix(g, "/") {
		g += "**"
	}
	for i := 0; i < len(g); i++ {
		switch ch := g[i]; {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case ch == '*':
			b.WriteString("[^/]*")
		case ch == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
