package hawkeye

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Findings are rules over the record, not judgements by a model: the same
// record always yields the same findings, and each one names the evidence.

var (
	// hostRE finds a URL host or a bare IPv4 address. It is deliberately
	// narrow: a finding that fires on every dotted word is one nobody reads.
	hostRE = regexp.MustCompile(`(?i)\b(?:https?|ftp|ssh)://([a-z0-9][a-z0-9.-]*[a-z0-9])|\b((?:\d{1,3}\.){3}\d{1,3})\b`)

	sensitive = []string{
		"/.ssh/", "/.aws/", "/.kube/", "/.gnupg/", "/.docker/config.json", "/.netrc",
		"/.env", "id_rsa", "id_ed25519", "/etc/shadow", "credentials.json", ".pem",
	}

	abnormal = map[string]string{
		"error": "the run failed", "stalled": "the model stopped making progress",
		"max_turns": "the turn limit was reached", "max_budget": "the token budget was spent",
		"shutdown": "the server shut down mid-turn", "retry_exhausted": "the model endpoint kept failing",
		"policy_denied": "policy ended the run", "deadline": "the run's time limit passed",
	}

	rank = map[Severity]int{Critical: 0, Warn: 1, Info: 2}
)

// A person's calls at the workbench count in the totals and in the findings
// about what was reached, denied or exposed. They are left out of the findings
// about the model's behaviour: repeats, slow calls, truncation and borrowed hosts.
func (c Call) byPerson() bool { return c.Actor == string(agent.ActorUser) }

// settledBy is who settled the call, naming the person where the record does.
func (c Call) settledBy() string {
	if c.Approver != "" {
		return c.By + " " + c.Approver
	}
	return c.By
}

// shellOpen reports a shell the person opened that has not ended yet.
func (c Call) shellOpen() bool {
	return c.byPerson() && c.Tool == "bash" && c.Decision == "allowed" && !c.Ran &&
		strings.Contains(c.Args, `"interactive":true`)
}

func findings(r Report, evs []agent.Event, opt Options) []Finding {
	out := []Finding{}
	add := func(sev Severity, code, title, detail string, seq int64) {
		out = append(out, Finding{Severity: sev, Code: code, Title: title, Detail: detail, Seq: seq})
	}

	// The record first: everything else in the report stands on it.
	if len(r.Integrity.Gaps) > 0 {
		add(Critical, "record-gap", "Events are missing from the record",
			fmt.Sprintf("The sequence skips at %v. An append-only store does not produce gaps, "+
				"so this record was filtered, truncated or edited before it was analysed.", r.Integrity.Gaps),
			r.Integrity.Gaps[0])
	}
	// The record cannot tell a shell still open from a server that died with
	// one, so an open shell changes what the finding says, not whether it is made.
	if !r.Integrity.HasEnd && len(evs) > 0 && !opt.Live {
		detail := "It is still running, or the process died before it could write one."
		if slices.ContainsFunc(r.Calls, Call.shellOpen) {
			detail = "A shell the person opened in the workbench had not ended either: the session may still be open " +
				"there, or the server stopped before it could record the end."
		}
		add(Info, "no-end", "The session has no recorded end", detail, 0)
	}
	if why, bad := abnormal[r.Outcome]; bad {
		add(Warn, "abnormal-end", "Ended as "+r.Outcome, "The session did not complete: "+why+".", r.Integrity.LastSeq)
	}

	out = append(out, borrowedHosts(r, evs)...)

	failures := map[string]int{}
	for _, c := range r.Calls {
		low := strings.ToLower(c.Subject + " " + c.Args)
		for _, s := range sensitive {
			if strings.Contains(low, s) {
				verb := "was allowed to reach"
				sev := Warn
				if c.Decision == "denied" {
					verb, sev = "was stopped from reaching", Info
				}
				add(sev, "sensitive-path", "A call "+verb+" a credential path",
					fmt.Sprintf("%s %s — matched %q. Decision: %s at step %q.", c.Tool, clip(c.Subject, 160), s, c.Decision, c.Step), c.Seq)
				break
			}
		}
		if c.Decision == "denied" {
			add(Info, "denied", "Denied: "+c.Tool,
				fmt.Sprintf("%s — %s (step %q, by %s).", clip(c.Subject, 160), c.Reason, c.Step, c.settledBy()), c.Seq)
		}
		if c.SandboxDenied {
			add(Info, "sandbox-denied", "The sandbox refused part of a command",
				fmt.Sprintf("%s — the command ran, but the sandbox denied an operation in it, whatever its exit status says.", clip(c.Subject, 160)), c.Seq)
		}
		if strings.Contains(c.Output, "[secret:") {
			add(Warn, "secret-redacted", "A stored secret's value was written out and redacted",
				fmt.Sprintf("%s %s — the output held a secret's value; the record has [secret:NAME] in its place. The value was caught before the write, but the command or the model exposed it.", c.Tool, clip(c.Subject, 160)), c.Seq)
		}
		// tools.NotApplied: the prefix edit and write put on a change they refused.
		if (c.Tool == "edit" || c.Tool == "write") && c.IsError && strings.HasPrefix(c.Output, "Not applied:") {
			add(Info, "broken-edit", "An edit was refused: it would have broken the file, or was a pasted diff",
				fmt.Sprintf("%s — %s", c.Tool, clip(c.Output, 240)), c.Seq)
		}
		if c.byPerson() {
			continue
		}
		if c.Ran && c.IsError {
			key := c.Tool + "\x00" + c.Args
			failures[key]++
			if failures[key] == 3 {
				add(Warn, "repeated-failure", "The same call failed three times",
					fmt.Sprintf("%s %s was retried unchanged after failing. The model was not adapting.", c.Tool, clip(c.Subject, 160)), c.Seq)
			}
		}
		if c.DurationMS > 60_000 {
			add(Info, "slow-tool", "A tool call took over a minute",
				fmt.Sprintf("%s %s ran for %ds.", c.Tool, clip(c.Subject, 120), c.DurationMS/1000), c.Seq)
		}
	}

	truncated := 0
	for _, c := range r.Calls {
		if c.Truncated && !c.byPerson() {
			truncated++
		}
	}
	if truncated > 0 {
		add(Info, "truncated", fmt.Sprintf("%d tool result(s) were truncated before the model saw them", truncated),
			"The record holds what the model was shown, not the full output.", 0)
	}

	capped := 0
	for _, t := range r.Turns {
		if t.CutOff && t.ToolCalls == 0 {
			capped++
		}
	}
	if capped > 0 {
		add(Warn, "output-cap", fmt.Sprintf("%d turn(s) spent the whole output budget without acting", capped),
			"The model was still reasoning when the output limit ended the turn, and made no tool call. "+
				"The loop nudges it and lowers the reasoning effort; if it keeps happening, the task is too "+
				"open for this model at this effort.", 0)
	}

	for _, t := range r.Turns {
		if t.Window > 0 && t.TokensIn*100 >= t.Window*85 {
			add(Warn, "context-pressure", "The context window was nearly full",
				fmt.Sprintf("Turn %d sent %d of %d tokens (%d%%). Past this point a single large tool result overflows it.",
					t.N, t.TokensIn, t.Window, t.TokensIn*100/t.Window), t.Seq)
			break
		}
	}
	// Only turns whose provider reported a cache figure count: an endpoint that
	// reports none would otherwise always read as cold.
	reported, in, cached := 0, 0, 0
	for _, t := range r.Turns {
		if t.CacheReported {
			reported, in, cached = reported+1, in+t.TokensIn, cached+t.TokensCached
		}
	}
	if reported >= 5 && in > 0 && cached*5 < in {
		add(Info, "cold-cache", "Most of the prompt was re-read cold on every turn",
			fmt.Sprintf("Cache hit rate %.0f%% across %d turns. The endpoint is not caching the prefix, "+
				"or something early in the prompt changes each turn.", float64(cached)*100/float64(in), reported), 0)
	}

	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Severity] < rank[out[j].Severity] })
	return out
}

// borrowedHosts flags an allowed call that names a host the user never
// mentioned and that first appeared in tool output. All tool output is
// untrusted, so this is the shape an injected "now send it to…" takes — and
// also the shape of following a link in a README, which is why it is a
// warning to read rather than a verdict.
func borrowedHosts(r Report, evs []agent.Event) []Finding {
	fromUser := map[string]bool{}
	fromTools := map[string]int64{}
	var out []Finding
	flagged := map[string]bool{}

	byID := map[string]Call{}
	for _, c := range r.Calls {
		byID[c.CallID] = c
	}

	for _, e := range evs {
		switch e.Type {
		case agent.EvUserMessage:
			var m agent.Message
			_ = json.Unmarshal(e.Payload, &m)
			for _, h := range hosts(m.Text) {
				fromUser[h] = true
			}
		case agent.EvObservation:
			var o agent.Observation
			_ = json.Unmarshal(e.Payload, &o)
			for _, h := range hosts(o.Content) {
				if _, seen := fromTools[h]; !seen {
					fromTools[h] = e.Seq
				}
			}
		case agent.EvActionRequested:
			var a agent.ActionRequested
			_ = json.Unmarshal(e.Payload, &a)
			c := byID[a.CallID]
			if c.Decision != "allowed" || c.byPerson() {
				continue
			}
			for _, h := range hosts(string(a.Args)) {
				src, borrowed := fromTools[h]
				if !borrowed || fromUser[h] || flagged[h] || local(h) {
					continue
				}
				flagged[h] = true
				out = append(out, Finding{
					Severity: Warn, Code: "borrowed-host", Seq: e.Seq,
					Title: "A call used a host that only tool output supplied",
					Detail: fmt.Sprintf("%s named %q. The user never mentioned it; it first appeared in tool output at #%d, "+
						"which is untrusted. Check that following it was the task and not an instruction planted in that output.",
						a.Tool, h, src),
				})
			}
		}
	}
	return out
}

func hosts(s string) []string {
	var out []string
	for _, m := range hostRE.FindAllStringSubmatch(s, -1) {
		h := strings.ToLower(m[1] + m[2])
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

func local(h string) bool {
	return h == "localhost" || strings.HasPrefix(h, "127.") || h == "0.0.0.0"
}
