package hawkeye

import "github.com/zybuu-ai/abhed/internal/ui"

// shown returns a copy of the report for a reader, with every record-derived
// string made visible: bidi, zero-width and control characters cannot reorder
// or hide text in the page or the terminal. The JSON report stays as recorded.
func shown(r Report) Report {
	v := ui.Visible
	r.SessionID, r.Prompt, r.Outcome = v(r.SessionID), v(r.Prompt), v(r.Outcome)
	r.Models = each(r.Models, func(s string) string { return v(s) })
	r.Turns = each(r.Turns, func(t Turn) Turn { t.Model, t.Error = v(t.Model), v(t.Error); return t })
	r.Calls = each(r.Calls, func(c Call) Call {
		c.CallID, c.Tool, c.Args, c.Subject, c.Decision, c.Step = v(c.CallID), v(c.Tool), v(c.Args), v(c.Subject), v(c.Decision), v(c.Step)
		c.By, c.Scope, c.Approver, c.GrantedScope, c.Reason = v(c.By), v(c.Scope), v(c.Approver), v(c.GrantedScope), v(c.Reason)
		c.Actor, c.Via, c.Output = v(c.Actor), v(c.Via), ui.VisibleOutput(c.Output)
		return c
	})
	if r.Policy.ByStep != nil {
		by := make(map[string]int, len(r.Policy.ByStep))
		for k, n := range r.Policy.ByStep {
			by[v(k)] += n
		}
		r.Policy.ByStep = by
	}
	r.Compactions = each(r.Compactions, func(c Compaction) Compaction { c.Trigger = v(c.Trigger); return c })
	r.Subagents = each(r.Subagents, func(s Subagent) Subagent {
		s.Description, s.Type, s.Reason, s.Session = v(s.Description), v(s.Type), v(s.Reason), v(s.Session)
		return s
	})
	r.SubagentActions = each(r.SubagentActions, func(a SubagentAction) SubagentAction {
		a.Session, a.Tool, a.Subject, a.Decision, a.Step = v(a.Session), v(a.Tool), v(a.Subject), v(a.Decision), v(a.Step)
		a.Reason, a.By, a.Approver = v(a.Reason), v(a.By), v(a.Approver)
		return a
	})
	r.Files = each(r.Files, func(f FileTouch) FileTouch { f.Path = v(f.Path); return f })
	r.Findings = each(r.Findings, func(f Finding) Finding {
		f.Code, f.Title, f.Detail = v(f.Code), v(f.Title), v(f.Detail)
		return f
	})
	return r
}

// each maps a slice into a new one, so the caller's report is left untouched.
func each[T any](in []T, f func(T) T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	for i, x := range in {
		out[i] = f(x)
	}
	return out
}
