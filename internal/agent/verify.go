package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Verified answers.
//
// No check can make a model right. What the harness can do is hold each
// statement of an answer against what this session actually observed: a
// statement shown as verified is tied to a tool result from this session that
// is still current and that a check found consistent with it; everything else
// is labelled unverified; and a statement the record contradicts is never
// shown as fact. The checks are deterministic (verify_check.go), so they hold
// with a weak model. A critic model may downgrade a verdict, never raise one.

// EvAnswerVerdict records what the checks found for one answer.
const EvAnswerVerdict EventType = "answer.verdict"

// TermUnverified ends a run whose answer, or whose completion contract, could
// not be verified where verification was required (strict mode, or a contract).
const TermUnverified TerminalReason = "unverified"

// VerifyMode is how a session treats its answers.
type VerifyMode string

const (
	// VerifyOff checks nothing.
	VerifyOff VerifyMode = "off"
	// VerifyLabel checks each answer and labels its statements; the run ends
	// as it would have. It makes a model call only when a claim is
	// contradicted: the agent's one revision turn.
	VerifyLabel VerifyMode = "label"
	// VerifyStrict also gives the agent one revision turn when a statement
	// about the session's work is not verified, and ends the run unverified
	// if it still is not.
	VerifyStrict VerifyMode = "strict"
)

// ParseVerifyMode reads a mode as a flag or setting spells it; "" is off.
func ParseVerifyMode(s string) (VerifyMode, error) {
	switch m := VerifyMode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", VerifyOff:
		return VerifyOff, nil
	case VerifyLabel, VerifyStrict:
		return m, nil
	}
	return "", fmt.Errorf("verify is off, label or strict, not %q", s)
}

func (m VerifyMode) rank() int {
	switch m {
	case VerifyLabel:
		return 1
	case VerifyStrict:
		return 2
	}
	return 0
}

// Tighter is the stricter of m and o.
func (m VerifyMode) Tighter(o VerifyMode) VerifyMode {
	if o.rank() > m.rank() {
		return o
	}
	if m == "" {
		return VerifyOff
	}
	return m
}

// Looser reports whether m checks less than o.
func (m VerifyMode) Looser(o VerifyMode) bool { return m.rank() < o.rank() }

// Verifier checks a loop's answers. Nil on a loop checks none.
type Verifier struct {
	Mode VerifyMode
	// AnswerTool offers the agent the answer tool, through which it gives its
	// answer with each claim's evidence. Without it, a text reply is split into
	// statements and each is matched against the session's results.
	AnswerTool bool
	// Critic, when set, has a model check the verified claims against their
	// evidence; it may only downgrade them.
	Critic *Critic
	// Contract is the operator's completion contract, or nil.
	Contract *Contract

	mu      sync.Mutex
	pending *givenAnswer
	revised bool
	last    *AnswerVerdict
	protect map[string]string
	// protectPartial is set when the run's start could not hash every
	// protected file.
	protectPartial bool
}

// givenAnswer is what the agent delivered through the answer tool.
type givenAnswer struct {
	Text   string  `json:"text"`
	Claims []Claim `json:"claims"`
}

// AnswerVerdict is the answer.verdict payload.
type AnswerVerdict struct {
	Mode VerifyMode `json:"mode"`
	// Source is answer_tool when the agent gave its claims and evidence,
	// text when its reply was split into statements by the harness.
	Source       string          `json:"source"`
	Claims       []ClaimVerdict  `json:"claims"`
	Verified     int             `json:"verified"`
	Unverified   int             `json:"unverified"`
	Contradicted int             `json:"contradicted"`
	Contract     *ContractResult `json:"contract,omitempty"`
	// Critic is "checked" when a critic reviewed the verified claims,
	// "unavailable" when it could not and they were downgraded.
	Critic string `json:"critic,omitempty"`
	// Revision is the note the agent was sent for its one revision turn;
	// another answer.verdict follows that turn.
	Revision string `json:"revision,omitempty"`
	// Outcome is verified when every statement about the session's work was
	// verified and any contract was met; otherwise unverified.
	Outcome string `json:"outcome"`
	Turn    int    `json:"turn"`
}

// Footer is the one-line summary a surface shows under the answer.
func (v AnswerVerdict) Footer() string {
	unv := fmt.Sprintf("%d unverified", v.Unverified)
	var notes []string
	k, e := v.Unchecked(), v.Explained()
	switch {
	case k > 0 && k == v.Unverified:
		notes = append(notes, "general knowledge")
	case k > 0:
		notes = append(notes, fmt.Sprintf("%d general knowledge", k))
	}
	if e > 0 {
		notes = append(notes, fmt.Sprintf("%d explanation", e))
	}
	if len(notes) > 0 {
		unv += " (" + strings.Join(notes, ", ") + ")"
	}
	s := fmt.Sprintf("%d verified · %s · %d contradicted", v.Verified, unv, v.Contradicted)
	if c := v.Contract; c != nil {
		if c.OK {
			s += " · contract met"
		} else {
			s += " · contract not met"
		}
	}
	return s
}

// Unchecked counts the unverified claims that are not about the session's
// work: general knowledge, and sentences with nothing to check.
func (v AnswerVerdict) Unchecked() int {
	n := 0
	for _, c := range v.Claims {
		if c.Verdict == ClaimUnverified && !c.LoadBearing {
			n++
		}
	}
	return n
}

// Explained counts the unverified claims that explain what a read showed.
// They are shown apart from unverified work, and still count against the
// outcome.
func (v AnswerVerdict) Explained() int {
	n := 0
	for _, c := range v.Claims {
		if c.Verdict == ClaimUnverified && c.Explains {
			n++
		}
	}
	return n
}

// LastVerdict is the latest answer.verdict this loop recorded, or nil.
func (l *Loop) LastVerdict() *AnswerVerdict {
	v := l.Verify
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.last == nil {
		return nil
	}
	cp := *v.last
	return &cp
}

// Install puts v on l: the answer tool into its registry and a line about it
// into its system prompt. Call it once the prompt is set.
func (v *Verifier) Install(l *Loop) {
	if v == nil || (v.Mode == VerifyOff && v.Contract == nil) {
		return
	}
	l.Verify = v
	if !v.AnswerTool || l.Tools == nil {
		return
	}
	if _, taken := l.Tools.Get(AnswerToolName); !taken {
		l.Tools.Add(&answerTool{v: v})
	}
	l.Config.SystemPrompt = strings.TrimRight(l.Config.SystemPrompt, "\n") + "\n\n" + answerPrompt
}

const answerPrompt = "# Answers are checked\n" +
	"Give your final answer by calling `answer`, not as plain text. Put the whole reply in `text`, and list each " +
	"factual statement in `claims` with `evidence`: the call_id of the tool result that shows it, and in `quote` the " +
	"exact words of that result it rests on. Mark what you know without a tool result as kind `knowledge`. Abhed checks " +
	"every claim against this session's record before the user sees it: a claim with no current evidence is shown as " +
	"unverified, and one the record contradicts is struck through. Saying you could not verify something is a good answer."

// AnswerToolName is the tool the agent gives a checked answer through.
const AnswerToolName = "answer"

type answerTool struct{ v *Verifier }

func (*answerTool) Name() string  { return AnswerToolName }
func (*answerTool) Mutates() bool { return false }
func (*answerTool) Description() string {
	return "Give your final answer, with the evidence for each claim in it. Call it once, at the end. `text` is " +
		"what the user reads. Each claim names the tool results (call_id) that show it, and quotes them where it can. " +
		"Claims are checked against this session's record; uncited or unsupported ones are shown as unverified."
}

func (*answerTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["text"],"properties":{
"text":{"type":"string","description":"The answer as the user should read it."},
"claims":{"type":"array","description":"Each factual statement in the answer.","items":{"type":"object","required":["text"],"properties":{
"text":{"type":"string","description":"The statement, as in the answer."},
"kind":{"type":"string","enum":["observed","derived","knowledge"],"description":"observed: a tool result shows it; derived: worked out from tool results; knowledge: not from this session."},
"evidence":{"type":"array","items":{"type":"string"},"description":"call_id of each tool result that shows it, or #seq of its record entry."},
"quote":{"type":"string","description":"The exact words in the cited result the statement rests on."}}}}}}`)
}

func (a *answerTool) Run(_ context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var g struct {
		Text   string          `json:"text"`
		Claims json.RawMessage `json:"claims"`
	}
	if err := json.Unmarshal(raw, &g); err != nil || strings.TrimSpace(g.Text) == "" {
		return tools.Result{Content: "answer needs `text`: the reply the user reads. Call answer again with it.", IsError: true}
	}
	claims := parseClaims(g.Claims)
	a.v.mu.Lock()
	a.v.pending = &givenAnswer{Text: g.Text, Claims: claims}
	a.v.mu.Unlock()
	return tools.Result{Content: "Answer received; it is checked against this session's record before it is shown.", Final: true}
}

// parseClaims reads the claims leniently: a weak model's evidence may be a
// number or a single string, and a malformed claim is kept as its text alone.
func parseClaims(raw json.RawMessage) []Claim {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []Claim
	for _, it := range items {
		var c struct {
			Text     string          `json:"text"`
			Kind     string          `json:"kind"`
			Evidence json.RawMessage `json:"evidence"`
			Quote    string          `json:"quote"`
		}
		if json.Unmarshal(it, &c) != nil {
			var s string
			if json.Unmarshal(it, &s) == nil && strings.TrimSpace(s) != "" {
				out = append(out, Claim{Text: s})
			}
			continue
		}
		if strings.TrimSpace(c.Text) == "" {
			continue
		}
		out = append(out, Claim{Text: c.Text, Kind: strings.ToLower(c.Kind), Evidence: evidenceRefs(c.Evidence), Quote: c.Quote})
		if len(out) == 60 {
			break
		}
	}
	return out
}

func evidenceRefs(raw json.RawMessage) []string {
	var list []any
	if json.Unmarshal(raw, &list) != nil {
		var one any
		if json.Unmarshal(raw, &one) != nil || one == nil {
			return nil
		}
		list = []any{one}
	}
	var out []string
	for _, x := range list {
		switch v := x.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				out = append(out, s)
			}
		case float64:
			out = append(out, fmt.Sprintf("#%d", int64(v)))
		}
	}
	return out
}

// beginAnswer starts a run's checks: no answer given yet, its one revision
// unused, and the protected files as they are now.
func (l *Loop) beginAnswer() {
	v := l.Verify
	if v == nil || l.depth > 0 {
		return
	}
	v.mu.Lock()
	v.pending, v.revised, v.last = nil, false, nil
	v.mu.Unlock()
	if c := v.Contract; c != nil && len(c.Protect) > 0 {
		snap, partial := c.snapshot(l.Session)
		v.mu.Lock()
		v.protect, v.protectPartial = snap, partial
		v.mu.Unlock()
	}
}

// checkAnswer checks a completed run's answer. again asks for the revision
// turn; otherwise reason is how the run ends.
func (l *Loop) checkAnswer(ctx context.Context) (again bool, reason TerminalReason) {
	v := l.Verify
	if v == nil || l.depth > 0 {
		return false, TermCompleted
	}
	v.mu.Lock()
	given := v.pending
	v.pending = nil
	revised := v.revised
	v.mu.Unlock()

	source, text, claims := "", "", []Claim(nil)
	if given != nil {
		source, text, claims = "answer_tool", given.Text, given.Claims
		// The reply the person reads is the answer's text: streamed, recorded
		// and kept as the agent's message, as a plain reply would be.
		l.record(EvAgentDelta, ActorAgent, Delta{Text: text, Seq: 1})
		if _, err := l.Recorder.Record(EvAgentMessage, ActorAgent, Trusted, Message{Text: text}); err != nil {
			l.noteRecordErr(err)
		}
		l.messages = append(l.messages, model.Message{Role: model.RoleAssistant, Content: text})
		if len(claims) == 0 {
			claims = textClaims(text)
		}
	} else if n := len(l.messages); n > 0 && l.messages[n-1].Role == model.RoleAssistant && len(l.messages[n-1].ToolCalls) == 0 {
		source, text = "text", l.messages[n-1].Content
		claims = textClaims(text)
	}
	// Nothing said: no claim to check, but the contract still gates the run.
	if source == "text" && strings.TrimSpace(text) == "" && v.Contract == nil {
		return false, TermCompleted
	}
	// Stopped as it ended: the answer stands as given, unchecked.
	// No answer to check: a plan put to the person or a structured result.
	// The contract gates an answer, so it waits for one too.
	if source == "" || ctx.Err() != nil {
		return false, TermCompleted
	}

	var cr *ContractResult
	// Plan mode changes nothing, so there is no work for a contract to gate.
	if v.Contract != nil && (l.Policy == nil || l.Policy.Mode != policy.ModePlan) {
		cr = l.runContract(ctx)
	}
	events, err := l.Recorder.store.Events(l.Recorder.sessionID)
	if err != nil {
		events = nil // checked against nothing, everything stays unverified
	}
	env := checkEnv{events: events, mutates: l.recordedMutates, workDir: l.workDir()}
	env.child = l.Recorder.store.Events
	if l.Session != nil {
		env.readFile = l.Session.ReadFile
	}
	verdicts, ix := checkClaims(env, claims)
	critic := ""
	if v.Critic != nil {
		critic = l.critique(ctx, v.Critic, verdicts, ix)
	}
	av := AnswerVerdict{Mode: v.Mode, Source: source, Claims: verdicts, Contract: cr, Critic: critic, Turn: l.turns}
	bad := 0
	for _, c := range verdicts {
		switch c.Verdict {
		case ClaimVerified:
			av.Verified++
		case ClaimContradicted:
			av.Contradicted++
		default:
			av.Unverified++
		}
		if c.LoadBearing && c.Verdict != ClaimVerified {
			bad++
		}
	}
	unmet := cr != nil && !cr.OK
	av.Outcome = ClaimVerified
	if bad > 0 || unmet {
		av.Outcome = ClaimUnverified
	}
	// A contract gates work, and plan mode makes none.
	gated := v.Mode == VerifyStrict || v.Contract != nil && (l.Policy == nil || l.Policy.Mode != policy.ModePlan)
	// In label mode a contradicted claim also earns the one revision: it is
	// the only case label mode spends a model call on.
	revise := (gated && av.Outcome != ClaimVerified) || (v.Mode == VerifyLabel && av.Contradicted > 0)
	if revise && !revised && l.turns < l.Config.MaxTurns && !l.Budget.Exhausted() {
		av.Revision = revisionNote(av, v.AnswerTool)
		v.mu.Lock()
		v.revised = true
		v.mu.Unlock()
	}
	l.record(EvAnswerVerdict, ActorSystem, av)
	v.mu.Lock()
	v.last = &av
	v.mu.Unlock()
	if av.Revision != "" {
		l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: av.Revision})
		return true, ""
	}
	if gated && av.Outcome != ClaimVerified {
		return false, TermUnverified
	}
	return false, TermCompleted
}

// textClaims are a plain reply's statements, as claims with nothing cited.
func textClaims(text string) []Claim {
	var out []Claim
	for _, s := range splitClaims(text) {
		c := Claim{Text: s}
		// A citation in the text, (#12), is the claim's evidence as the
		// answer tool's would be.
		for _, m := range citeRe.FindAllStringSubmatch(s, -1) {
			for _, n := range m[1:] {
				if n != "" {
					c.Evidence = append(c.Evidence, "#"+n)
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// recordedMutates says whether a recorded call could have changed state, by
// the tool's own account; a tool no longer registered is assumed to.
func (l *Loop) recordedMutates(tool string, args json.RawMessage) bool {
	if l.Tools == nil {
		return true
	}
	t, ok := l.Tools.Get(tool)
	if !ok {
		return true
	}
	return tools.MutatesCall(t, args)
}

func (l *Loop) workDir() string {
	if l.Session == nil {
		return ""
	}
	return l.Session.WorkDir()
}

// revisionNote is what the agent is told for its one revision turn.
func revisionNote(av AnswerVerdict, tool bool) string {
	var b strings.Builder
	b.WriteString("Abhed checked your answer against this session's record before showing it, and could not confirm all of it.\n")
	n := 0
	for _, c := range av.Claims {
		if !c.LoadBearing || c.Verdict == ClaimVerified {
			continue
		}
		n++
		if n > 12 {
			b.WriteString("…and more.\n")
			break
		}
		fmt.Fprintf(&b, "- %s: %q — %s\n", c.Verdict, clipRunes(c.Text, 200), c.Reason)
	}
	if c := av.Contract; c != nil && !c.OK {
		for _, r := range c.Require {
			if !r.OK {
				fmt.Fprintf(&b, "- the completion check `%s` did not pass: %s\n", clipRunes(r.Command, 120), r.Reason)
			}
		}
		if len(c.Protected) > 0 {
			fmt.Fprintf(&b, "- protected files changed, which the operator does not allow: %s\n", strings.Join(c.Protected, ", "))
		}
		if c.ProtectPartial {
			b.WriteString("- the protected files are too many to check; this is the operator's to narrow, not yours to fix\n")
		}
	}
	b.WriteString("Check what you can with a tool, then give your answer again")
	if tool {
		b.WriteString(" through `answer`, citing the call_id of each result")
	}
	b.WriteString(". Correct or remove anything the record contradicts, and say plainly what you could not verify. " +
		"This is your one revision.")
	return b.String()
}
