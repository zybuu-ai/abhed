package hawkeye

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// outputKeep bounds how much of a tool result a report carries. The record
// keeps all of it; a report that embedded every byte would be the record.
const outputKeep = 4000

// Options is what the record alone cannot say.
type Options struct {
	// Live is set when the session is still running where the report is made.
	Live bool
}

// Analyze derives a report from a session's events. Events may arrive in any
// order; they are read in sequence order.
func Analyze(sessionID string, events []agent.Event) Report {
	return AnalyzeWith(sessionID, events, Options{})
}

// AnalyzeWith is Analyze with what the caller knows beyond the record.
func AnalyzeWith(sessionID string, events []agent.Event, opt Options) Report {
	evs := append([]agent.Event(nil), events...)
	ordered := sort.SliceIsSorted(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })

	r := Report{
		SessionID: sessionID,
		Outcome:   "running",
		Policy:    PolicyStats{ByStep: map[string]int{}},
		Findings:  []Finding{},
	}
	r.Integrity = integrity(evs, ordered)
	r.Totals.Events = len(evs)
	if len(evs) == 0 {
		return r
	}
	r.Started, r.Ended = evs[0].CreatedAt, evs[len(evs)-1].CreatedAt
	r.Totals.DurationMS = r.Ended.Sub(r.Started).Milliseconds()

	calls := map[string]*Call{}
	var order []string
	files := map[string]*FileTouch{}
	var ended agent.SessionEnded

	for _, e := range evs {
		switch e.Type {
		case agent.EvUserMessage:
			if r.Prompt == "" {
				var m agent.Message
				_ = json.Unmarshal(e.Payload, &m)
				r.Prompt = clip(m.Text, 400)
			}

		case agent.EvModelCall:
			var m agent.ModelCall
			_ = json.Unmarshal(e.Payload, &m)
			r.Turns = append(r.Turns, Turn{
				N: len(r.Turns) + 1, Seq: e.Seq, TokensIn: m.TokensIn, TokensOut: m.TokensOut,
				TokensCached: m.TokensCached, CacheReported: m.CacheReported,
				Window: m.ContextWindow, FirstTokenMS: m.FirstTokenMS,
				LatencyMS: m.LatencyMS, ToolCalls: m.ToolCalls, Error: m.Error, CutOff: m.CutOff,
			})

		case agent.EvActionRequested:
			var a agent.ActionRequested
			_ = json.Unmarshal(e.Payload, &a)
			c := &Call{
				Seq: e.Seq, CallID: a.CallID, Tool: a.Tool, Args: clip(string(a.Args), 2000),
				Subject: policy.Subject(a.Tool, a.Args), Decision: "pending", Reason: a.Reason,
				Actor: string(e.Actor),
			}
			calls[a.CallID] = c
			order = append(order, a.CallID)
			touch(files, a.Tool, a.Args)

		case agent.EvActionApproved, agent.EvActionDenied:
			var d map[string]string
			_ = json.Unmarshal(e.Payload, &d)
			c := calls[d["call_id"]]
			if c == nil {
				continue
			}
			c.Step, c.Reason, c.By, c.Scope = d["step"], d["reason"], d["by"], d["scope"]
			c.Approver, c.GrantedScope = d["approver"], d["granted_scope"]
			c.Decision = "allowed"
			if e.Type == agent.EvActionDenied {
				c.Decision = "denied"
			}
			// A record written before denials carried "by" says only the actor.
			if c.By == "" && e.Type == agent.EvActionDenied {
				c.By = agent.ByPolicy
				if e.Actor == agent.ActorUser {
					c.By = agent.ByReviewer
				}
			}

		case agent.EvObservation:
			var o agent.Observation
			_ = json.Unmarshal(e.Payload, &o)
			if e.Trust == agent.Untrusted {
				r.Totals.Untrusted++
			}
			c := calls[o.CallID]
			if c == nil {
				continue
			}
			c.Ran, c.IsError, c.ExitCode = true, o.IsError, o.ExitCode
			// Only where a sandbox was in force: on the host the same words are the system's.
			c.SandboxDenied = c.Tool == "bash" && o.Sandbox != "" && o.Sandbox != "none" && sandboxRefused(o.Content)
			c.Truncated, c.DurationMS = o.Truncated, o.DurationMS
			c.Output, c.OutputLen = clip(o.Content, outputKeep), len(o.Content)

		case agent.EvCompactDone:
			var k agent.Compaction
			if json.Unmarshal(e.Payload, &k) == nil && k.BeforeTokens > 0 {
				r.Compactions = append(r.Compactions, Compaction{
					Seq: e.Seq, Trigger: k.Trigger, Before: k.BeforeTokens, After: k.AfterTokens,
				})
			}

		case agent.EvContextOffloaded:
			var o agent.Offloaded
			_ = json.Unmarshal(e.Payload, &o)
			r.Offloads = append(r.Offloads, Offload{Seq: e.Seq, Results: o.Results, Before: o.BeforeTokens, After: o.AfterTokens})

		case agent.EvSubagentSpawned:
			var p struct {
				Description string `json:"description"`
				AgentType   string `json:"agent_type"`
				Depth       int    `json:"depth"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			r.Subagents = append(r.Subagents, Subagent{
				Seq: e.Seq, Description: p.Description, Type: p.AgentType, Depth: p.Depth,
			})

		case agent.EvSubagentReturn:
			var p struct {
				Description string `json:"description"`
				Reason      string `json:"reason"`
				Turns       int    `json:"turns"`
				TokensIn    int    `json:"tokens_in"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			for i := range r.Subagents {
				s := &r.Subagents[i]
				if !s.Returned && s.Description == p.Description {
					s.Returned, s.Reason, s.Turns, s.TokensIn = true, p.Reason, p.Turns, p.TokensIn
					break
				}
			}

		case agent.EvSessionEnded:
			_ = json.Unmarshal(e.Payload, &ended)
			r.Outcome = string(ended.Reason)
		}
	}

	for _, id := range order {
		r.Calls = append(r.Calls, *calls[id])
	}
	for _, f := range files {
		r.Files = append(r.Files, *f)
	}
	sort.Slice(r.Files, func(i, j int) bool { return r.Files[i].Path < r.Files[j].Path })

	r.totals(ended)
	r.Findings = findings(r, evs, opt)
	return r
}

func (r *Report) totals(ended agent.SessionEnded) {
	t := &r.Totals
	t.ToolCalls = len(r.Calls)
	for _, c := range r.Calls {
		t.ToolMS += c.DurationMS
		if c.Tool == "recall" {
			t.Recalls++
		}
		switch c.Decision {
		case "allowed":
			r.Policy.Allowed++
		case "denied":
			r.Policy.Denied++
		}
		if c.By == agent.ByReviewer {
			r.Policy.Reviewer++
		}
		if c.Step != "" {
			r.Policy.ByStep[c.Step]++
		}
	}
	for _, turn := range r.Turns {
		t.TokensIn += turn.TokensIn
		t.TokensOut += turn.TokensOut
		t.TokensCached += turn.TokensCached
		t.ModelMS += turn.LatencyMS
		if turn.TokensIn > t.PeakContext {
			t.PeakContext = turn.TokensIn
		}
		if turn.Window > t.Window {
			t.Window = turn.Window
		}
	}
	t.Turns = len(r.Turns)
	// A record written before per-turn accounting existed still carries the
	// session's own totals, which is better than reporting zero.
	if len(r.Turns) == 0 {
		t.Turns, t.TokensIn, t.TokensOut = ended.Turns, ended.TokensIn, ended.TokensOut
		t.TokensCached, t.PeakContext, t.Window = ended.TokensCached, ended.ContextTokens, ended.ContextWindow
	}
	if t.TokensIn > 0 {
		t.CacheHitRate = float64(t.TokensCached) / float64(t.TokensIn)
	}
}

func integrity(evs []agent.Event, ordered bool) Integrity {
	in := Integrity{Ordered: ordered}
	if len(evs) == 0 {
		return in
	}
	in.FirstSeq, in.LastSeq = evs[0].Seq, evs[len(evs)-1].Seq
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq != evs[i-1].Seq+1 {
			in.Gaps = append(in.Gaps, evs[i-1].Seq+1)
		}
	}
	for _, e := range evs {
		if e.Type == agent.EvSessionEnded {
			in.HasEnd = true
		}
	}
	return in
}

// sandboxRefused spots the note bash adds to its result when the sandbox
// denied an operation (tools.sandboxHint). The command ran; part of it did not.
func sandboxRefused(output string) bool {
	return strings.Contains(output, "NOTE: the sandbox denied this operation") ||
		strings.Contains(output, "NOTE: Abhed's sandbox blocks")
}

// touch counts file access from the arguments of the file tools. bash is left
// out on purpose: guessing paths out of a shell command would report things
// that did not happen.
func touch(files map[string]*FileTouch, tool string, args json.RawMessage) {
	if tool != "read" && tool != "write" && tool != "edit" {
		return
	}
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(args, &a) != nil || a.Path == "" {
		return
	}
	f := files[a.Path]
	if f == nil {
		f = &FileTouch{Path: a.Path}
		files[a.Path] = f
	}
	if tool == "read" {
		f.Reads++
	} else {
		f.Writes++
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Back off to a rune boundary so a cut never lands inside a character.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
