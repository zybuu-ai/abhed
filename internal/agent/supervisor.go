package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/monitor"
)

const (
	// EvSupervisorCorrection records what the supervisor told the model when
	// it refused a call: the concern, why, and what to do instead.
	EvSupervisorCorrection EventType = "supervisor.correction"
	// EvSupervisorReplan records the note that restates the task after a run
	// of refused or failed calls.
	EvSupervisorReplan EventType = "supervisor.replan"
)

// SupervisorNote is the supervisor's verdict on a call, as an approval card shows it.
type SupervisorNote struct {
	Decision  string `json:"decision"`
	Code      string `json:"code"`
	Rationale string `json:"rationale,omitempty"`
	Advice    string `json:"advice,omitempty"`
	Judge     string `json:"judge,omitempty"`
}

// SupervisorCorrection is the text a refused call's answer carried to the model.
type SupervisorCorrection struct {
	CallID   string `json:"call_id"`
	Code     string `json:"code"`
	Decision string `json:"decision"`
	Text     string `json:"text"`
}

// SupervisorReplan is the re-plan note, after Failures refused or failed calls in a row.
type SupervisorReplan struct {
	Failures int    `json:"failures"`
	Text     string `json:"text"`
}

// replanAfter is how many refused or failed calls in a row bring the note.
const replanAfter = 3

// supervision is the supervisor's memory in one loop: the verdicts of the
// turn's calls, the run of failures, and the plan the person approved.
type supervision struct {
	mu       sync.Mutex
	verdicts map[string]monitor.Verdict
	streak   int
	plan     string
	// said are the person's own messages in the conversation, as the record
	// holds them; loaded marks them read from it, and stale a conversation
	// replaced since (a fork, a rewind, a restore).
	said   []string
	loaded bool
	// parent is the loop that spawned this one: its person's words are this
	// subagent's, and its task, which a model wrote, is not.
	parent *Loop
}

// SetApprovedPlan keeps the plan the person approved, which the re-plan note restates.
func (l *Loop) SetApprovedPlan(text string) {
	l.sup.mu.Lock()
	l.sup.plan = strings.TrimSpace(text)
	l.sup.mu.Unlock()
}

func (l *Loop) noteVerdict(callID string, v monitor.Verdict) {
	l.sup.mu.Lock()
	defer l.sup.mu.Unlock()
	if l.sup.verdicts == nil {
		l.sup.verdicts = map[string]monitor.Verdict{}
	}
	l.sup.verdicts[callID] = v
}

func (l *Loop) verdictOf(callID string) (monitor.Verdict, bool) {
	l.sup.mu.Lock()
	defer l.sup.mu.Unlock()
	v, ok := l.sup.verdicts[callID]
	return v, ok
}

// clearVerdicts forgets the last turn's verdicts.
func (l *Loop) clearVerdicts() {
	l.sup.mu.Lock()
	clear(l.sup.verdicts)
	l.sup.mu.Unlock()
}

// supervisorNote is the verdict on a call for its approval card, if one was given.
func (l *Loop) supervisorNote(callID string) *SupervisorNote {
	v, ok := l.verdictOf(callID)
	if !ok {
		return nil
	}
	return &SupervisorNote{Decision: string(v.Decision), Code: v.Code, Rationale: v.Rationale, Advice: v.Advice, Judge: v.Version}
}

// correction is what a refused call tells the model when the supervisor had
// a concern with it, recorded as supervisor.correction; "" when it had none.
func (l *Loop) correction(callID string, d policy.Decision) string {
	v, ok := l.verdictOf(callID)
	if !ok || v.Code == "" || v.Code == monitor.CodeOK {
		return ""
	}
	text := v.Correction()
	l.record(EvSupervisorCorrection, ActorSystem, SupervisorCorrection{CallID: callID, Code: v.Code, Decision: string(d), Text: text})
	return text
}

// remitAndSaid are the first thing the person asked and everything they
// said: only messages recorded as theirs. A compaction summary, a revision
// or parse note, a nudge and a subagent's task are text a model or the
// harness wrote, and name nothing on the person's behalf.
func (l *Loop) remitAndSaid() (string, string) {
	said := l.personSaid()
	if len(said) == 0 {
		return "", ""
	}
	return said[0], strings.Join(said, "\n") + "\n"
}

// personSaid is the person's messages, oldest first, a subagent's parent's first.
func (l *Loop) personSaid() []string {
	l.sup.mu.Lock()
	parent := l.sup.parent
	if !l.sup.loaded {
		l.sup.said, l.sup.loaded = l.recordedSaid(parent != nil), true
	}
	own := append([]string(nil), l.sup.said...)
	l.sup.mu.Unlock()
	if parent == nil {
		return own
	}
	return append(parent.personSaid(), own...)
}

// recordedSaid reads the person's messages from the live conversation in the
// record. A subagent's own prompt is its parent model's task, which has no
// queue id; a message the person sent it while it ran has one.
func (l *Loop) recordedSaid(sub bool) []string {
	if l.Recorder == nil || l.Recorder.store == nil {
		return nil
	}
	evs, err := l.Recorder.store.Events(l.Recorder.sessionID)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range Live(evs) {
		if e.Type != EvUserMessage || e.Actor != ActorUser {
			continue
		}
		var m Message
		if json.Unmarshal(e.Payload, &m) != nil || sub && m.QueueID == "" {
			continue
		}
		out = append(out, m.Text)
	}
	return out
}

// noteSaid keeps a message the person just sent; queued says it came while a run went on.
func (l *Loop) noteSaid(text string, queued bool) {
	l.sup.mu.Lock()
	defer l.sup.mu.Unlock()
	if l.sup.loaded && (l.sup.parent == nil || queued) {
		l.sup.said = append(l.sup.said, text)
	}
}

// forgetSaid has the person's words read again from the record, after the
// conversation was replaced.
func (l *Loop) forgetSaid() {
	l.sup.mu.Lock()
	l.sup.loaded, l.sup.said = false, nil
	l.sup.mu.Unlock()
}

// seen reports whether a tool's output in this conversation showed a text,
// leaving out what came from outside: the web, an MCP server, a command
// that reached the network (see monitor.Untrusted).
func (l *Loop) seen() func(string) bool {
	var once sync.Once
	var text string
	return func(s string) bool {
		once.Do(func() {
			web := map[string]bool{}
			var b strings.Builder
			for _, m := range l.messages {
				for _, tc := range m.ToolCalls {
					if monitor.Untrusted(tc.Name, string(tc.Args)) {
						web[tc.ID] = true
					}
				}
				if m.Role == model.RoleTool && !web[m.ToolCallID] {
					b.WriteString(m.Content)
					b.WriteByte('\n')
				}
			}
			text = b.String()
		})
		return s != "" && strings.Contains(text, s)
	}
}

// supervisorCase is what the supervisor is shown about one call.
func (l *Loop) supervisorCase(call model.ToolCall, mutates bool, before policy.Result) monitor.Case {
	remit, said := l.remitAndSaid()
	args := string(call.Args)
	l.monitorMu.Lock()
	thoughts := append([]string(nil), l.reasoning...)
	recent := append([]monitor.Recent(nil), l.recentCalls...)
	l.monitorMu.Unlock()
	seen := l.seen()
	var roots []string
	if l.Session != nil {
		roots = append([]string{l.Session.Root}, l.Session.Roots...)
	}
	c := monitor.Case{
		Remit: remit, Said: said, Reasoning: thoughts,
		Tool: call.Name, Args: args, Mutates: mutates, Provisional: before,
		Provenance: monitor.TraceSeen(args, said, seen), Recent: recent,
		Roots: roots, Seen: seen,
	}
	if l.Policy != nil {
		c.DeclaredRead = l.Policy.RemoteReadOf(call.Name, call.Args)
	}
	l.sup.mu.Lock()
	c.Plan = l.sup.plan
	l.sup.mu.Unlock()
	// A bound runbook's steps are what the person approved, word for word.
	if rb, _, ok := l.BoundRunbook(); ok {
		var b strings.Builder
		b.WriteString(c.Plan)
		for _, st := range rb.Steps {
			fmt.Fprintf(&b, "\n%s %s", st.Tool, st.Target)
			var args map[string]any
			_ = json.Unmarshal(st.Args, &args)
			for _, v := range args {
				if t, ok := v.(string); ok {
					b.WriteString(" " + t)
				}
			}
		}
		c.Plan = strings.TrimSpace(b.String())
	}
	return c
}

// replan counts refused and failed calls in a row and, every replanAfter of
// them, adds a note to the last one's answer that restates the task and the
// approved plan, so a model that has lost the way plans again from them.
func (l *Loop) replan(results []callOutcome) {
	if l.Monitor == nil {
		return
	}
	for i := range results {
		if !results[i].set {
			continue
		}
		if !results[i].result.IsError {
			l.sup.mu.Lock()
			l.sup.streak = 0
			l.sup.mu.Unlock()
			continue
		}
		l.sup.mu.Lock()
		l.sup.streak++
		n, plan := l.sup.streak, l.sup.plan
		l.sup.mu.Unlock()
		if n%replanAfter != 0 {
			continue
		}
		remit, _ := l.remitAndSaid()
		note := fmt.Sprintf("Supervisor: the last %d calls were refused or failed. Stop and plan again from the task. The task: %q.",
			n, clipRunes(strings.TrimSpace(remit), 400))
		if plan != "" {
			note += fmt.Sprintf(" The approved plan: %q.", plan)
		}
		note += " Inspect before you change anything, pick the next step from the task, or say what is blocking you and stop."
		l.record(EvSupervisorReplan, ActorSystem, SupervisorReplan{Failures: n, Text: note})
		results[i].result.Content += "\n\n[" + note + "]"
	}
}
