package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// The deterministic answer checker. No model is asked anything here: each
// claim is compared with the session's own record and the files as they are
// now, and comes out verified, unverified or contradicted.

// Claim verdicts.
const (
	ClaimVerified     = "verified"
	ClaimUnverified   = "unverified"
	ClaimContradicted = "contradicted"
)

// Claim kinds, as the answer tool takes them.
const (
	KindObserved  = "observed"
	KindDerived   = "derived"
	KindKnowledge = "knowledge"
	// KindStatement is a sentence of a reply given as text, with nothing in
	// it a check could compare.
	KindStatement = "statement"
)

// Claim is one statement of an answer and what the agent says shows it.
type Claim struct {
	Text string `json:"text"`
	Kind string `json:"kind,omitempty"`
	// Evidence names tool results by call_id, or record entries by seq.
	Evidence []string `json:"evidence,omitempty"`
	// Quote is the exact text in the cited output the claim rests on.
	Quote string `json:"quote,omitempty"`
}

// ClaimVerdict is what the checks found for one claim.
type ClaimVerdict struct {
	N       int    `json:"n"`
	Text    string `json:"text"`
	Kind    string `json:"kind"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	// Evidence is the record entries (seq) the verdict rests on.
	Evidence []int64 `json:"evidence,omitempty"`
	// Cited is what the agent cited, as it wrote it.
	Cited []string `json:"cited,omitempty"`
	// Conflict is the record entry that contradicts the claim.
	Conflict *Conflict `json:"conflict,omitempty"`
	// LoadBearing marks a claim about this session's work, as opposed to
	// general knowledge or a sentence with nothing to check.
	LoadBearing bool `json:"load_bearing,omitempty"`
	// Checked lists what was compared, such as "number 12" or "exit 0 of #31".
	Checked []string `json:"checked,omitempty"`
	// Critic is the critic's downgrade, when it made one.
	Critic string `json:"critic,omitempty"`
	// Explains marks an unverified claim that explains what a read showed.
	// It is shown apart from unverified work, and changes nothing else: the
	// claim keeps its kind and LoadBearing, so the answer's outcome and any
	// revision are what they would be without it.
	Explains bool `json:"explains,omitempty"`
}

// Conflict is the evidence a contradicted claim is shown with.
type Conflict struct {
	Seq     int64  `json:"seq,omitempty"`
	CallID  string `json:"call_id,omitempty"`
	Excerpt string `json:"excerpt"`
}

// checkEnv is what the checker reads besides the claims.
type checkEnv struct {
	events []Event
	// mutates reports whether a recorded call could change state; nil
	// treats every call but the known readers as one that could.
	mutates func(tool string, args json.RawMessage) bool
	// child reads a subagent's own record by its session; nil reads none.
	child func(session string) ([]Event, error)
	// readFile reads a file as it is now; nil checks no file:line cite.
	readFile func(path string) ([]byte, error)
	// workDir resolves a relative path.
	workDir string
}

// obsRec is one tool result in the record, with the call that produced it.
type obsRec struct {
	// seq is the parent record's entry a claim cites it by: its own, or for
	// a subagent's result the parent's subagent.returned.
	seq int64
	// pos orders it among every call the checks read, a subagent's included.
	pos     float64
	child   string // the subagent session whose record holds it, "" for this session's
	reqSeq  int64
	callID  string
	tool    string
	command string // bash's command
	path    string // a file tool's path
	args    string // every string argument, decoded
	argMap  map[string]string
	content string
	exit    *int
	isError bool
	person  bool // made by a person at the workbench
	via     string
	ev      *evidenceText
	inspect bool // a command that only reads
	// selfStale: the call's line changes something after printing what the
	// result shows, so its output may predate that change.
	selfStale bool
	// line is what a shell call's line does; cwd the folder it ran in.
	line lineFacts
	cwd  string
}

// mutation is a recorded call that may have changed what later output shows.
type mutation struct {
	seq     int64
	pos     float64
	tool    string
	path    string // set for write and edit: the one file changed
	command string
	person  bool
	failed  bool
	// known: every file the call may have changed is in targets, absolute.
	known   bool
	targets []string
	// writer: a file tool, or a shell line read in full as known writers
	// alone, whose exit says whether the change was made.
	writer bool
	// covers are folders a formatter rewrote; child is the subagent session
	// the call was in, "" for this one's.
	covers []string
	child  string
	// created: a write that made its file ("Created …").
	created bool
	// text is what the call was given (an edit's text, a command), where a
	// change that names no file looks for the code it names.
	text string
}

type recordIndex struct {
	obs       []*obsRec
	byCall    map[string]*obsRec
	bySeq     map[int64]*obsRec // by observation seq and by request seq
	types     map[int64]EventType
	mutations []mutation
	// subagents is set when a subagent ran whose own record could not be
	// read, so its work is not in the index.
	subagents bool
	// workDir is the workspace, against which a claim's relative path is read.
	workDir string
}

// readers are the built-in tools that never change anything.
var readers = map[string]bool{
	"read": true, "grep": true, "glob": true, "recall": true, "todo": true, AnswerToolName: true,
	"web_fetch": true, "web_search": true, "task_status": true, "ls": true,
}

// maxChildDepth bounds how deep subagents' records are read.
const maxChildDepth = 3

func indexRecord(env checkEnv) *recordIndex {
	ix := &recordIndex{byCall: map[string]*obsRec{}, bySeq: map[int64]*obsRec{}, types: map[int64]EventType{}, workDir: env.workDir}
	ix.add(env, env.events, "", nil, 0, map[string]bool{})
	sort.SliceStable(ix.obs, func(i, j int) bool { return ix.obs[i].pos < ix.obs[j].pos })
	sort.SliceStable(ix.mutations, func(i, j int) bool { return ix.mutations[i].pos < ix.mutations[j].pos })
	return ix
}

// add indexes one record. For this session's own, at is nil and an entry's
// place is its seq. A subagent's record is placed at the parent's entry for
// its return (at): its calls are taken as happening just before it, in
// their own order, and a claim cites them by that entry.
func (ix *recordIndex) add(env checkEnv, events []Event, session string, at *int64, depth int, seen map[string]bool) {
	type req struct {
		a      ActionRequested
		seq    int64
		person bool
	}
	var top int64
	for _, e := range events {
		top = max(top, e.Seq)
	}
	place := func(seq int64) (int64, float64) {
		if at == nil {
			return seq, float64(seq)
		}
		return *at, float64(*at) - 1 + float64(seq)/float64(top+1)
	}
	reqs := map[string]req{}
	children := map[string]int64{}
	var order []string
	for _, e := range Live(events) {
		if at == nil {
			ix.types[e.Seq] = e.Type
		}
		switch e.Type {
		case EvActionRequested:
			var a ActionRequested
			if json.Unmarshal(e.Payload, &a) == nil {
				reqs[a.CallID] = req{a: a, seq: e.Seq, person: e.Actor == ActorUser}
			}
		case EvSubagentSpawned, EvSubagentReturn, EvSubagentNotice:
			var p struct {
				Session string `json:"session"`
			}
			if json.Unmarshal(e.Payload, &p) != nil || p.Session == "" || p.Session == session {
				continue
			}
			if _, ok := children[p.Session]; !ok {
				order = append(order, p.Session)
			}
			// The latest of its entries places it: its return, once there is one.
			children[p.Session] = e.Seq
		case EvObservation:
			var o Observation
			if json.Unmarshal(e.Payload, &o) != nil || o.NotRun {
				continue
			}
			r := reqs[o.CallID]
			seq, pos := place(e.Seq)
			rec := &obsRec{seq: seq, pos: pos, child: session, reqSeq: r.seq, callID: o.CallID, tool: o.Tool, content: o.Content,
				exit: o.ExitCode, isError: o.IsError, person: r.person, via: r.a.Via}
			var args struct {
				Command string `json:"command"`
				Path    string `json:"path"`
			}
			_ = json.Unmarshal(r.a.Args, &args)
			rec.command, rec.path, rec.args, rec.argMap = args.Command, args.Path, argText(r.a.Args), argMap(r.a.Args)
			var line lineFacts
			if o.Tool == "bash" || o.Tool == "ssh" {
				// The folder the bash tool ran the command in, as the record
				// holds it; a remote command's, or one not recorded, is not
				// known, and its relative files are not either.
				dir := ""
				if o.Tool == "bash" && at == nil {
					dir = o.Cwd
				}
				line = readLine(args.Command, dir)
				rec.inspect, rec.selfStale = line.parsed && line.inspect, line.selfStale
				rec.line, rec.cwd = line, o.Cwd
			}
			mut := false
			switch {
			case readers[o.Tool]:
			case o.Tool == "bash":
				mut = !rec.inspect
			case env.mutates != nil:
				mut = env.mutates(o.Tool, r.a.Args)
			default:
				mut = true
			}
			// The contract's own commands check the work; counted as changes,
			// they would put everything before them out of date, their own
			// earlier pass included.
			if r.a.Via == ContractVia {
				mut = false
			}
			// A refused edit changed nothing; a failed command may have.
			fileTool := o.Tool == "write" || o.Tool == "edit"
			// A refused edit changed nothing, but is kept, failed, so a claim
			// about its file is left open; a failed command may have changed things.
			if mut || o.OutcomeUnknown {
				m := mutation{seq: seq, pos: pos, tool: o.Tool, command: args.Command, person: r.person, failed: o.IsError || o.OutcomeUnknown || (o.ExitCode != nil && *o.ExitCode != 0),
					text: string(r.a.Args), created: o.Tool == "write" && strings.HasPrefix(o.Content, "Created ")}
				switch {
				case fileTool:
					m.path, m.known, m.targets, m.writer = args.Path, args.Path != "", []string{args.Path}, true
				case o.Tool == "bash" || o.Tool == "ssh":
					m.known, m.targets, m.writer, m.covers = line.parsed && line.known, line.targets, line.parsed && line.writer, line.covers
				}
				m.child = session
				ix.mutations = append(ix.mutations, m)
			}
			if o.OutcomeUnknown {
				continue
			}
			ix.obs = append(ix.obs, rec)
			if _, taken := ix.byCall[o.CallID]; !taken || at == nil {
				ix.byCall[o.CallID] = rec
			}
			if at == nil {
				ix.bySeq[e.Seq] = rec
				if r.seq > 0 {
					ix.bySeq[r.seq] = rec
				}
			}
		}
	}
	// A subagent's work is in its own record, read when the checks can.
	for _, id := range order {
		anchor := children[id]
		if at != nil {
			anchor = *at
		}
		var evs []Event
		if env.child != nil && depth < maxChildDepth && !seen[id] {
			seen[id] = true
			evs, _ = env.child(id)
		}
		if len(evs) == 0 {
			ix.subagents = true
			continue
		}
		ix.add(env, evs, id, &anchor, depth+1, seen)
	}
}

// staleBy is the first later mutation that could have changed what o shows,
// or nil. A file read is changed by an edit of that file or by a command that
// writes; a command's output by any change at all; anything else (a remote
// command, a cluster read) by a later call that may change state outside a file.
func (ix *recordIndex) staleBy(o *obsRec) *mutation {
	for i := range ix.mutations {
		m := &ix.mutations[i]
		// A line that changes something after printing what its result
		// shows is out of date by its own change.
		if m.pos == o.pos && o.selfStale {
			return m
		}
		if m.pos <= o.pos {
			continue
		}
		fileEdit := m.tool == "write" || m.tool == "edit"
		if fileEdit && m.failed {
			continue // a refused edit changed nothing
		}
		switch o.tool {
		case "read":
			// A change whose files are all known leaves a read of another alone.
			if !m.known || touches(m.targets, o.path) {
				return m
			}
		case "grep", "glob", "bash":
			return m
		default:
			if !fileEdit {
				return m
			}
		}
	}
	return nil
}

// touches reports a target that is p, or a folder p is beneath.
func touches(targets []string, p string) bool {
	if p == "" {
		return true
	}
	p = filepath.Clean(p)
	for _, t := range targets {
		t = filepath.Clean(t)
		if t == p || strings.HasPrefix(p, t+"/") {
			return true
		}
	}
	return false
}

// lastChange is the last recorded call that may have changed something.
func (ix *recordIndex) lastChange() *mutation {
	for i := len(ix.mutations) - 1; i >= 0; i-- {
		m := &ix.mutations[i]
		if (m.tool == "write" || m.tool == "edit") && m.failed {
			continue // a refused edit changed nothing
		}
		return m
	}
	return nil
}

func (o *obsRec) evidence() *evidenceText {
	if o.ev == nil {
		o.ev = prepareEvidence(o.content, o.testRun())
	}
	return o.ev
}

// testRun reports a shell line with a test run in it, whose bare "9 passed"
// counts tests.
func (o *obsRec) testRun() bool {
	for _, c := range o.line.cmds {
		if commandKind(c.words) == "test" {
			return true
		}
	}
	return false
}

// usable is the evidence a claim of the model's may rest on: results of the
// agent's own calls and of the completion contract's, not a person's.
func (o *obsRec) usable() bool { return !o.person && o.tool != AnswerToolName }

// outcome of one check.
type checkOutcome int

const (
	neutral checkOutcome = iota
	matched
	missing
	conflicted
)

type finding struct {
	out      checkOutcome
	what     string // what was compared, for Checked
	why      string // for an unverified or contradicted reason
	seq      int64
	conflict *Conflict
	pooled   bool     // matched in a tool result, not in the record's calls or the files
	bare     bool     // a number with nothing to say what it counts
	unit     string   // the unit or key a count or keyed finding is about
	said     []string // the outcome, action or state words of a count finding
	call     bool     // matched in the record's calls, which can show an event
	kind     string   // for a call match: "change", "run" or "look"
}

// checkClaims checks each claim against the record. Claims from the answer
// tool carry their own citations; claims split from a text reply are matched
// against every result still current.
func checkClaims(env checkEnv, claims []Claim) ([]ClaimVerdict, *recordIndex) {
	ix := indexRecord(env)
	out := make([]ClaimVerdict, 0, len(claims))
	for i, c := range claims {
		v := checkOne(env, ix, c)
		v.N = i + 1
		out = append(out, v)
	}
	return out, ix
}

// checkOne checks a claim. One that says a thing did not happen, or is not
// so, is never verified: output showing the thing does not show its absence.
func checkOne(env checkEnv, ix *recordIndex, c Claim) ClaimVerdict {
	shown := c.Text
	c.Text = lookLed(pastLabeled(c.Text))
	v := checkWhole(env, ix, c)
	v.Text = shown
	// Each clause is judged on its own too, and the claim is verified only
	// when every one is: "I read `sum.go` and inserted `continue`" needs the
	// read and the change.
	if v.Verdict == ClaimVerified && c.Quote == "" {
		if parts := clauses(c.Text); len(parts) > 1 {
			for _, p := range parts {
				// A clause that says nothing about the work has nothing to show.
				if !saysWork(p) {
					continue
				}
				cv := checkWhole(env, ix, Claim{Text: p, Kind: c.Kind, Evidence: c.Evidence})
				if cv.Verdict != ClaimVerified {
					v.Verdict = ClaimUnverified
					v.Reason = fmt.Sprintf("in %q: %s", clipRunes(p, 60), cv.Reason)
					break
				}
			}
		}
	}
	// Explaining what a read showed is shown apart, and only shown: it never
	// makes an answer verified.
	if explainsOn && v.Verdict == ClaimUnverified && v.LoadBearing && explains(ix, c) {
		v.Explains = true
	}
	return v
}

// checkWhole checks a claim as one statement.
func checkWhole(env checkEnv, ix *recordIndex, c Claim) ClaimVerdict {
	v := checkClaim(env, ix, c)
	if v.Verdict == ClaimVerified && extractFacts(citeRe.ReplaceAllString(c.Text, " ")).negated {
		v.Verdict = ClaimUnverified
		v.Reason = "it says something did not happen or is not so, which a result showing the thing does not show"
	}
	// A state ("healthy", "down") or a deed ("we deployed") the matched
	// output does not itself say is not shown by its other words matching.
	if v.Verdict == ClaimVerified {
		if why := unsaid(ix, c, v); why != "" {
			v.Verdict, v.Reason = ClaimUnverified, why
		}
	}
	return v
}

// citeRe is a citation written in a reply's text: (#12), [#12] or record #12.
var citeRe = regexp.MustCompile(`\((?:see\s+)?#(\d+)\)|\[#(\d+)\]|\brecord #(\d+)\b`)

func checkClaim(env checkEnv, ix *recordIndex, c Claim) ClaimVerdict {
	f := extractFacts(citeRe.ReplaceAllString(c.Text, " "))
	kind := c.Kind
	switch kind {
	case KindObserved, KindDerived, KindKnowledge:
	case "":
		kind = KindObserved
		// A sentence that says something was done or changed is about the work
		// even with nothing else in it; a participle alone ("a compiled
		// language") is not.
		workEvent := f.deed || f.observe || f.changeWord || f.changeAct && f.event != "" || f.overclaim
		if !f.any() && !workEvent && c.Quote == "" && len(c.Evidence) == 0 {
			kind = KindStatement
		}
	default:
		kind = KindObserved
	}
	// A "knowledge" claim about this session's own work is checked as one.
	if kind == KindKnowledge && (f.success || f.changed || len(f.ran) > 0 || len(f.fileLines) > 0) {
		kind = KindObserved
	}
	v := ClaimVerdict{Text: c.Text, Kind: kind, Cited: c.Evidence}
	// "Set `DEBUG` to `true`" is advice in a session that changed nothing,
	// with a condition or purpose ("when you're ready", "to apply it"), or
	// with a verb no call of this session showed ("Restart the server").
	if f.imperative && len(c.Evidence) == 0 {
		why := ""
		low := normText(backtickRe.ReplaceAllString(c.Text, " "))
		switch {
		case !ix.changedAny():
			why = "it reads as advice, and this session changed nothing"
		case adviceTailRe.MatchString(low) && !saysPast(low[:adviceTailRe.FindStringIndex(low)[0]]):
			// A past form before the tail ("Update `Sum`, which I changed, to
			// …") reports work; one in the tail ("once this is deployed") does not.
			why = "it reads as advice: it says when or what for"
		default:
			if m := unshownVerbRe.FindStringSubmatch(low); m != nil && !ix.ranWord(m[1]) {
				why = "it reads as advice: no call this session did " + m[1]
			}
		}
		if why != "" {
			v.Kind, v.Verdict, v.Reason = KindStatement, ClaimUnverified, why
			return v
		}
	}
	if kind == KindKnowledge || kind == KindStatement {
		v.Verdict = ClaimUnverified
		v.Reason = "general knowledge; not checked against this session"
		if kind == KindStatement {
			v.Reason = "nothing in it could be checked against this session"
		}
		return v
	}
	v.LoadBearing = true

	// The evidence pool: what was cited, or, with nothing cited, every
	// current result of the agent's own calls.
	var pool []*obsRec
	var findings []finding
	cited := len(c.Evidence) > 0
	if cited {
		for _, ref := range c.Evidence {
			o, why := ix.resolve(ref)
			if o == nil {
				findings = append(findings, finding{out: missing, why: why})
				continue
			}
			if m := ix.staleBy(o); m != nil {
				findings = append(findings, finding{out: missing,
					why: fmt.Sprintf("#%d predates a later change (#%d, %s)", o.seq, m.seq, mutationLabel(*m))})
				continue
			}
			pool = append(pool, o)
		}
		// Every citation failed: the claim stands on nothing.
		if len(pool) == 0 {
			return settle(v, findings, "none of its cited evidence is a current result in this session")
		}
	} else {
		for _, o := range ix.obs {
			if o.usable() && ix.staleBy(o) == nil {
				pool = append(pool, o)
			}
		}
	}

	const none = "nothing in it could be matched to the cited output"
	if cited {
		return settle(v, append(findings, judgeFacts(env, ix, f, c.Quote, pool, true)...), none)
	}
	// Uncited, a claim is verified only by one result that shows all of it,
	// and never by a bare number alone: a 3 somewhere in the output is not
	// the 3 the claim means.
	for i := len(pool) - 1; i >= 0; i-- {
		fs := judgeFacts(env, ix, f, c.Quote, pool[i:i+1], false)
		if got := settle(v, fs, none); got.Verdict == ClaimVerified && !bareOnly(fs) {
			return got
		}
	}
	fs := judgeFacts(env, ix, f, c.Quote, pool, false)
	got := settle(v, fs, none)
	if got.Verdict == ClaimVerified && pooledMatch(fs) {
		got.Verdict = ClaimUnverified
		got.Reason = "what it says appears only in pieces, or as bare numbers, across this session's results; " +
			"no one result shows it (cite the one that does)"
	}
	return got
}

// judgeFacts compares a claim's facts with the pool of results it may rest
// on, and with the record and the files where a fact is about those.
func judgeFacts(env checkEnv, ix *recordIndex, f facts, quote string, pool []*obsRec, cited bool) []finding {
	var findings []finding
	if quote != "" {
		q := normText(quote)
		if o := firstWith(pool, func(e *evidenceText) bool { return strings.Contains(e.norm, q) }); o != nil {
			findings = append(findings, finding{out: matched, what: "quote in #" + itoa(o.seq), seq: o.seq, pooled: true})
		} else if cited {
			findings = append(findings, citedMiss(pool, "its quote"))
		} else {
			findings = append(findings, finding{out: missing, why: "its quote is not in any current result"})
		}
	}
	// A file:line cite is checked against the file now; what it quotes there
	// needs no tool result as well.
	atLine := map[string]bool{} // literals found other than in a tool result
	for _, fl := range f.fileLines {
		fd, lit := checkFileLine(env, fl, f.literals)
		findings = append(findings, fd)
		if fd.out == matched {
			atLine[lit] = true
		}
	}
	var changedText *evidenceText // what a matched change put in
	// What the agent says it changed or ran is found in its calls, not in
	// their output.
	if f.changed && f.negated {
		// "I did not change X": never verified by what was done; contradicted
		// only by a cited write or edit of X.
		for _, p := range f.paths {
			if cited {
				findings = append(findings, ix.negatedChange(pool, p))
			} else {
				findings = append(findings, finding{out: missing, why: "a claim that something was not done is not shown by what was"})
			}
		}
	} else if (f.changed || f.changeWord || (f.changeAct || f.changeEvent) && (f.deed || f.event != "")) && !f.observeOnly && !f.proof && (!f.success || f.changed || f.deed) && len(f.ran) == 0 && !f.negated {
		// A change, in any wording, is read from the calls: by the change to
		// the file it names, whose own changed text must hold the code it
		// names; or, naming none, by a change whose text holds that code.
		pathTok := map[string]bool{}
		for _, p := range f.paths {
			pathTok[normText(p)] = true
		}
		code := false
		for _, t := range f.tokens {
			code = code || !pathTok[normText(t)] && !pathTok[normText(rawPath(t))]
		}
		for _, p := range f.paths {
			fd := ix.changedPath(p)
			fd.kind = "change"
			if fd.out == matched && code {
				nfd, put := ix.changeNamed(f, ix.inWorkspace(p))
				if nfd.out != matched {
					fd = nfd
				} else {
					for _, t := range f.tokens {
						markToken(atLine, t)
					}
					changedText = put
				}
			}
			fd.call = fd.out == matched
			findings = append(findings, fd)
			if fd.out == matched {
				atLine[normText(p)] = true
			}
		}
		if len(f.paths) == 0 {
			fd, put := ix.changeNamed(f, "")
			fd.kind = "change"
			findings = append(findings, fd)
			if fd.out == matched {
				for _, t := range f.tokens {
					markToken(atLine, t)
				}
				changedText = put
			}
		}
	}
	// What the agent says it looked at is read from its reads and searches.
	if f.observe && !f.negated {
		fd := ix.observed(f)
		// A look shows only itself: never another event in the sentence.
		fd.call, fd.kind = fd.out == matched, "look"
		findings = append(findings, fd)
		if fd.out == matched {
			for _, p := range f.paths {
				atLine[normText(p)] = true
			}
			for _, t := range f.tokens {
				markToken(atLine, t)
			}
		}
	}
	// A check or a release the agent says it did ("I tested", "we
	// deployed") is no change, and no output shows that it was done.
	if f.proof && len(f.ran) == 0 && !f.negated && !f.success {
		findings = append(findings, finding{out: missing, why: "it says what was done, which only the calls could show, and none is read as that"})
	}
	for _, cmd := range f.ran {
		fd := ix.ranCommand(cmd)
		fd.call, fd.kind = fd.out == matched, "run"
		findings = append(findings, fd)
		if fd.out == matched {
			atLine[normText(cmd)] = true
		}
	}
	// A write or an edit reports the file it changed: whether a path was
	// changed is the record's to say (changedPath), not that report's.
	pathLit := map[string]bool{}
	changedLit := map[string]string{}
	for _, p := range f.paths {
		pathLit[normText(p)] = true
		// A statement about a file this session changed, other than that it
		// was changed, is about its content, which no output naming it shows.
		if why := ix.namedByChange(p); !f.changed && why != "" {
			changedLit[normText(p)] = why
		}
	}
	var notFileTools []*obsRec
	for _, o := range pool {
		if o.tool != "write" && o.tool != "edit" {
			notFileTools = append(notFileTools, o)
		}
	}
	for _, lit := range f.literals {
		switch {
		case atLine[lit]:
		case changedLit[lit] != "":
			findings = append(findings, finding{out: missing, why: changedLit[lit] + ", and output naming it does not show what it holds"})
		case pathLit[lit] && len(notFileTools) < len(pool):
			fd := ix.literal(notFileTools, lit, false)
			if fd.out != matched {
				fd = finding{out: missing, why: fmt.Sprintf("%q is only in the report of a write or an edit, which shows no more than that the call ran", clipRunes(lit, 60))}
			}
			findings = append(findings, fd)
		default:
			findings = append(findings, ix.literal(pool, lit, cited))
		}
	}
	var unitFindings []finding
	// A count said of a change is the change's: read from what it was given
	// ("--replicas=3"), never from output after it.
	cpool, ccited := pool, cited
	if changedText != nil {
		cpool, ccited = []*obsRec{{ev: changedText}}, false
	}
	for _, cf := range f.counted {
		fd := ix.counted(cpool, cf, ccited)
		fd.unit, fd.said = cf.unit, closedOf(cf.quals)
		unitFindings = append(unitFindings, fd)
	}
	keyedOK := map[string]bool{}
	for _, kf := range f.keyed {
		if fd, ok := ix.keyed(cpool, kf, ccited); ok {
			fd.unit = kf.unit
			unitFindings = append(unitFindings, fd)
			keyedOK[kf.n] = true
		}
	}
	findings = append(findings, oneValueMet(unitFindings)...)
	for _, n := range f.numbers {
		if countedNumber(f.counted, n) || keyedOK[n] {
			continue // judged with its unit above
		}
		if changedText != nil {
			if !changedText.numbers[n] {
				findings = append(findings, finding{out: missing, why: "the number " + n + " is not in what the change was given"})
			}
			continue
		}
		findings = append(findings, ix.number(pool, n, cited))
	}
	if f.success {
		fd := ix.success(pool, f.testish, f.scope, cited && f.wholeRun, cited)
		if fd.out != matched && f.compiles {
			// A whole test run that passed compiled the code ("it compiles"),
			// when its runner compiles (go, cargo, maven, gradle, dotnet).
			if t := ix.success(pool, "test", nil, cited && f.wholeRun, cited); t.out == matched && ix.bySeq[t.seq] != nil && compilingTestRe.MatchString(ix.bySeq[t.seq].command) {
				fd = t
			}
		}
		if o := ix.bySeq[fd.seq]; fd.out == matched && f.testish == "lint" && o != nil && lintProblemsRe.MatchString(strings.ToLower(o.content)) {
			fd = finding{out: missing, why: "the lint run reports problems"}
		}
		if o := ix.bySeq[fd.seq]; fd.out == matched && f.noWarnings && o != nil && runWarnRe.MatchString(o.content) {
			fd = finding{out: missing, why: "the run reports warnings"}
		}
		if o := ix.bySeq[fd.seq]; fd.out == matched && f.buildTarget != "" && (o == nil || !targetShown(f.buildTarget, o.command)) {
			fd = finding{out: missing, why: "the run does not build for " + f.buildTarget}
		}
		fd.call, fd.kind = fd.out == matched, "run"
		findings = append(findings, fd)
	}
	// "No issues", "looks good": only a check run that passed shows it.
	if f.overclaim {
		// "Lint is clean" needs a lint run; "everything works" a test or a
		// build; anything else a lint or a test run. A run covers the whole
		// workspace or every file changed; a lint run must report no problems.
		kinds := []string{"lint", "test"}
		switch f.overKind {
		case "lint":
			kinds = []string{"lint"}
		case "works", "build":
			kinds = []string{"test", "build"}
		}
		scopes := [][]string{nil}
		if changed := ix.changedFiles(); len(changed) > 0 {
			scopes = append(scopes, changed)
		}
		fd := finding{out: missing, why: "it says the work is clean, which only a " + strings.Join(kinds, " or ") + " run that passed after the last change shows"}
	search:
		for _, k := range kinds {
			for _, sc := range scopes {
				got := ix.success(pool, k, sc, false, cited)
				if got.out != matched {
					continue
				}
				if o := ix.bySeq[got.seq]; k == "lint" && o != nil && lintProblemsRe.MatchString(strings.ToLower(o.content)) {
					fd = finding{out: missing, why: "the lint run reports problems"}
					continue
				}
				if o := ix.bySeq[got.seq]; f.noWarnings && o != nil && runWarnRe.MatchString(o.content) {
					fd = finding{out: missing, why: "the run reports warnings"}
					continue
				}
				if o := ix.bySeq[got.seq]; f.buildTarget != "" && (o == nil || !targetShown(f.buildTarget, o.command)) {
					fd = finding{out: missing, why: "the run does not build for " + f.buildTarget}
					continue
				}
				fd = got
				break search
			}
		}
		fd.call, fd.kind = fd.out == matched, "run"
		findings = append(findings, fd)
	}
	// A sentence that reports an event is shown only by the calls: output
	// matching its words is not enough.
	if f.event != "" {
		// Each kind of event the sentence reports needs a call of that kind:
		// a run for a pass, a look for a look, a change for anything else.
		have := map[string]bool{}
		for _, fd := range findings {
			if fd.call && fd.out == matched {
				have[fd.kind] = true
			}
		}
		for _, k := range f.eventKinds {
			if !have[k] {
				findings = append(findings, finding{out: missing,
					why: fmt.Sprintf("it says something happened (%q), which only the calls can show, and none is read as that", f.event)})
				break
			}
		}
	}
	return findings
}

// oneValueMet: a statement that gives one unit or key more than one value
// ("3 tests failed before the fix, 0 after", "from port 8080 to port 9090")
// is not contradicted by the others when the result shows one of them.
func oneValueMet(fs []finding) []finding {
	// Values are of one thing when they share the unit and a word said of
	// it, or neither says one: "9 tests passed" met does not excuse "2 tests
	// failed".
	oneThing := func(a, b finding) bool {
		return a.unit == b.unit && (len(a.said) == 0 && len(b.said) == 0 || sharesQual(a.said, b.said))
	}
	for i, f := range fs {
		if f.out != conflicted {
			continue
		}
		met := false
		for _, g := range fs {
			met = met || g.out == matched && oneThing(f, g)
		}
		if met {
			fs[i] = finding{out: missing, unit: f.unit,
				why: "it gives " + f.unit + " more than one value, and the result shows one of them: " + f.why}
		}
	}
	return fs
}

// bareOnly reports findings whose matches from tool output are all bare numbers.
func bareOnly(fs []finding) bool {
	seen := false
	for _, f := range fs {
		if f.out == matched {
			if !f.bare {
				return false
			}
			seen = true
		}
	}
	return seen
}

// pooledMatch reports a match that came from tool output rather than from
// the record's calls or the files.
func pooledMatch(fs []finding) bool {
	for _, f := range fs {
		if f.out == matched && f.pooled {
			return true
		}
	}
	return false
}

// settle turns the findings into one verdict: any conflict contradicts, any
// gap leaves it unverified, and only a claim with something matched and
// nothing missing is verified.
func settle(v ClaimVerdict, fs []finding, none string) ClaimVerdict {
	var gaps []string
	seen := map[int64]bool{}
	for _, f := range fs {
		switch f.out {
		case conflicted:
			if v.Verdict != ClaimContradicted {
				v.Verdict, v.Reason, v.Conflict = ClaimContradicted, f.why, f.conflict
			}
		case missing:
			gaps = append(gaps, f.why)
		case matched:
			v.Checked = append(v.Checked, f.what)
			if f.seq > 0 && !seen[f.seq] {
				seen[f.seq] = true
				v.Evidence = append(v.Evidence, f.seq)
			}
		}
	}
	switch {
	case v.Verdict == ClaimContradicted:
	case len(gaps) > 0:
		v.Verdict, v.Reason = ClaimUnverified, strings.Join(dedupe(gaps), "; ")
	case len(v.Checked) == 0:
		v.Verdict, v.Reason = ClaimUnverified, none
	default:
		v.Verdict, v.Reason = ClaimVerified, "matches "+strings.Join(seqList(v.Evidence), ", ")
		if len(v.Evidence) == 0 {
			v.Reason = "matches " + strings.Join(v.Checked, ", ")
		}
	}
	return v
}

var seqRefRe = regexp.MustCompile(`^#?(\d+)$`)

// resolve finds a cited tool result, or says why it cannot be used.
func (ix *recordIndex) resolve(ref string) (*obsRec, string) {
	ref = strings.TrimSpace(ref)
	if o, ok := ix.byCall[ref]; ok {
		if !o.usable() {
			return nil, ref + " is not a result of the agent's own calls"
		}
		return o, ""
	}
	if m := seqRefRe.FindStringSubmatch(ref); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		if o, ok := ix.bySeq[n]; ok && o.usable() {
			return o, ""
		}
		if t, ok := ix.types[n]; ok {
			return nil, fmt.Sprintf("#%d is a %s, not a tool result", n, t)
		}
		return nil, fmt.Sprintf("it cites #%d, which is not in this session's record", n)
	}
	return nil, fmt.Sprintf("it cites %q, which is not in this session's record", ref)
}

func firstWith(pool []*obsRec, ok func(*evidenceText) bool) *obsRec {
	// The latest first: it is the one that describes the present.
	for i := len(pool) - 1; i >= 0; i-- {
		if ok(pool[i].evidence()) {
			return pool[i]
		}
	}
	return nil
}

func (ix *recordIndex) literal(pool []*obsRec, lit string, cited bool) finding {
	if o := firstWith(pool, func(e *evidenceText) bool { return e.hasLiteral(lit) }); o != nil {
		return finding{out: matched, what: fmt.Sprintf("%q in #%d", lit, o.seq), seq: o.seq, pooled: true}
	}
	if cited {
		return citedMiss(pool, fmt.Sprintf("%q", clipRunes(lit, 60)))
	}
	return finding{out: missing, why: fmt.Sprintf("%q is not in %s", clipRunes(lit, 60), poolName(cited))}
}

// citedMiss is what the claim says that its cited output does not hold.
// Absence is never a contradiction: output silent on a thing does not say
// otherwise, so the claim is left unverified.
func citedMiss(pool []*obsRec, what string) finding {
	var seqs []int64
	for _, o := range pool {
		seqs = append(seqs, o.seq)
	}
	return finding{out: missing, why: fmt.Sprintf("%s is not in the cited %s", what, strings.Join(seqList(seqs), ", "))}
}

func (ix *recordIndex) number(pool []*obsRec, n string, cited bool) finding {
	if o := firstWith(pool, func(e *evidenceText) bool { return e.numbers[n] }); o != nil {
		return finding{out: matched, what: "number " + n + " in #" + itoa(o.seq), seq: o.seq, pooled: true, bare: true}
	}
	if cited {
		return citedMiss(pool, "the number "+n)
	}
	return finding{out: missing, why: "the number " + n + " is not in " + poolName(cited)}
}

// counted checks a number with the word it counts, and with what it says of
// it. A count that names an outcome, an action or a state ("12 tests
// passed", "added 3 tests", "3 replicas are ready") is met only by one that
// names it too, and contradicted only by a cited count of the same unit that
// names it with another number, and never when words after the claim's
// count narrow it ("3 tests in the auth package"). A plain count is met by
// any count of the unit with its number, and contradicted by none.
func (ix *recordIndex) counted(pool []*obsRec, cf countFact, cited bool) finding {
	label := cf.n + " " + cf.unit
	if cf.unit == "%" {
		label = cf.n + "%"
	}
	// The M of "N of M": the claim says N of them are so, which a count of M
	// shows nothing of either way.
	if cf.denom {
		return finding{out: missing, unit: cf.unit, why: fmt.Sprintf("%s is the whole that part of it is counted against", label)}
	}
	said := closedOf(cf.quals)
	var other, elsewise *obsRec
	var otherAt, elseAt countAt
	for i := len(pool) - 1; i >= 0; i-- {
		e := pool[i].evidence()
		for _, at := range e.counts[cf.unit] {
			same := sharesQual(said, at.quals)
			if len(said) == 0 {
				same = len(cf.quals) == 0 || len(at.quals) == 0 || sharesQual(cf.quals, at.quals)
			}
			switch {
			case at.n == cf.n && same:
				return finding{out: matched, what: label + " in #" + itoa(pool[i].seq), seq: pool[i].seq, pooled: true}
			case at.n != cf.n && len(said) > 0 && same && !cf.scoped && other == nil:
				other, otherAt = pool[i], at
			case elsewise == nil:
				elsewise, elseAt = pool[i], at
			}
		}
	}
	if other != nil && cited {
		return finding{out: conflicted, seq: other.seq,
			why:      fmt.Sprintf("it says %s; the cited #%d says %s", strings.Join(append([]string{label}, sayingOf(cf.unit, said)...), " "), other.seq, otherAt.excerpt),
			conflict: &Conflict{Seq: other.seq, CallID: other.callID, Excerpt: otherAt.excerpt}}
	}
	if other != nil {
		return finding{out: missing, why: fmt.Sprintf("it says %s; #%d says %s", label, other.seq, otherAt.excerpt)}
	}
	// The output counts the unit, but not as the claim does: a bare number
	// elsewhere in it is not the claim's.
	if elsewise != nil {
		return finding{out: missing, why: fmt.Sprintf("it says %s; #%d counts it otherwise: %s", label, elsewise.seq, elseAt.excerpt)}
	}
	// A count that says what of the unit needs output that says it too.
	if len(said) > 0 {
		return finding{out: missing, why: "no result counts " + strings.Join(append([]string{label}, sayingOf(cf.unit, said)...), " ")}
	}
	// The output does not count that thing: the bare number decides.
	return ix.number(pool, cf.n, cited)
}

// sayingOf are the words said of a unit, less the one the unit already is
// ("0 failed", not "0 failed fail").
func sayingOf(unit string, said []string) []string {
	var out []string
	for _, w := range said {
		if closedQuals[unit] != w {
			out = append(out, w)
		}
	}
	return out
}

// keyed checks "port 8080" where the output labels the word, as "port: 8080"
// does; ok is false when no result labels it, and the bare number decides.
func (ix *recordIndex) keyed(pool []*obsRec, kf countFact, cited bool) (finding, bool) {
	var other *obsRec
	var otherAt countAt
	for i := len(pool) - 1; i >= 0; i-- {
		for _, at := range pool[i].evidence().labels[kf.unit] {
			if at.n == kf.n {
				return finding{out: matched, what: kf.unit + " " + kf.n + " in #" + itoa(pool[i].seq), seq: pool[i].seq, pooled: true}, true
			}
			if other == nil {
				other, otherAt = pool[i], at
			}
		}
	}
	if other == nil {
		return finding{}, false
	}
	// Uncited, the other label may be another thing's (another server's
	// port); cited, so may it be when the claim's number is there too.
	held := false
	for _, o := range pool {
		held = held || o.evidence().numbers[kf.n]
	}
	if !cited || held {
		return finding{out: missing, why: fmt.Sprintf("it says %s %s; #%d says %s", kf.unit, kf.n, other.seq, otherAt.excerpt)}, true
	}
	return finding{out: conflicted, seq: other.seq,
		why:      fmt.Sprintf("it says %s %s; the cited #%d says %s", kf.unit, kf.n, other.seq, otherAt.excerpt),
		conflict: &Conflict{Seq: other.seq, CallID: other.callID, Excerpt: otherAt.excerpt}}, true
}

func countedNumber(cs []countFact, n string) bool {
	for _, c := range cs {
		if c.n == n {
			return true
		}
	}
	return false
}

// success checks a claim that tests passed or a command succeeded: a command
// must have ended with exit 0 after the last change, and none of its kind
// since may have failed.
func (ix *recordIndex) success(pool []*obsRec, testish string, scope []string, contradict, cited bool) finding {
	// A run counts only as the whole line, or the last command of an &&
	// chain at its top with only cd before it, so its exit is the run's; of
	// the claim's own kind (a test run is no build); in this session's
	// workspace; neither filtering, listing nor compiling without running;
	// and covering, by path, what the claim names, or the whole workspace.
	scopeDirs := claimScope(scope, ix.workDir)
	fits := func(o *obsRec) ([]scopePat, bool) {
		if o.tool != "bash" || o.exit == nil || !o.usable() || o.child != "" || testish == "" || ix.workDir == "" {
			return nil, false
		}
		lf := o.line
		if !lf.shape || !lf.leadCd || lf.finalCwd == "" || filepath.Clean(o.cwd) != filepath.Clean(ix.workDir) ||
			commandKind(lf.final) != testish || !harmlessEnv(lf.finalAssigns) {
			return nil, false
		}
		return runScope(lf.final, lf.finalCwd)
	}
	if contradict {
		// Contradicted by a cited run of the same kind and the same scope
		// that failed on its own: no runner around it, exit 1; only when the
		// claim is that the run passed, not a count of what passed in it.
		for _, o := range pool {
			pats, ok := fits(o)
			if ok && *o.exit == 1 && !o.line.finalWrapped && sameScope(pats, scopeDirs, ix.workDir) {
				return finding{out: conflicted, seq: o.seq,
					why:      fmt.Sprintf("the cited run #%d `%s` exited 1", o.seq, clipRunes(o.command, 60)),
					conflict: &Conflict{Seq: o.seq, CallID: o.callID, Excerpt: "exit 1: " + tailLines(o.content)}}
			}
		}
	}
	last := ix.lastChange()
	// The latest such run that covers the claim, after the last possible
	// change (a runner that is itself that change comes after it).
	for i := len(ix.obs) - 1; i >= 0; i-- {
		o := ix.obs[i]
		pats, ok := fits(o)
		if !ok || !coversScope(pats, scopeDirs, ix.workDir) {
			continue
		}
		if last != nil && o.pos < last.pos {
			break
		}
		if *o.exit != 0 {
			return finding{out: missing, why: fmt.Sprintf("the last such run, #%d, exited %d", o.seq, *o.exit)}
		}
		if cited && !inPool(pool, o) {
			return finding{out: missing, why: fmt.Sprintf("#%d is the run that shows it, and is not cited", o.seq)}
		}
		return finding{out: matched, what: fmt.Sprintf("exit 0 of #%d after the last change", o.seq), seq: o.seq, pooled: true}
	}
	if testish == "" {
		return finding{out: missing, why: "a success of no named kind is not read from a run"}
	}
	return finding{out: missing, why: "no whole " + testish + " run covering it succeeded after the last change"}
}

func inPool(pool []*obsRec, o *obsRec) bool {
	for _, p := range pool {
		if p == o {
			return true
		}
	}
	return false
}

// changedPath checks "I changed X" against the calls that changed files.
func (ix *recordIndex) changedPath(p string) finding {
	// Verified by the last call that changed p, as the claim writes it, when
	// it succeeded: a write or an edit of p, or a shell line in this session's
	// workspace read in full as known writers, exit 0, whose destination is p
	// (or a folder a formatter rewrote that holds p). A claim naming a folder
	// is met by a change beneath it. A later call naming p that failed or was
	// refused leaves it open. Uncited, it is never contradicted.
	want := ix.inWorkspace(p)
	if want == "" {
		return finding{out: missing, why: "where " + p + " is cannot be told"}
	}
	names := func(t string) bool {
		t = filepath.Clean(t)
		return t == want || strings.HasPrefix(t, want+"/")
	}
	var hit, failedAfter *mutation
	for i := range ix.mutations {
		m := &ix.mutations[i]
		if m.person {
			continue
		}
		named := false
		for _, t := range m.targets {
			named = named || names(t)
		}
		for _, c := range m.covers {
			named = named || (c != "" && (filepath.Clean(c) == want || strings.HasPrefix(want, filepath.Clean(c)+"/")))
		}
		if !named {
			continue
		}
		switch {
		case m.failed:
			if hit != nil {
				failedAfter = m
			}
		case ix.localChange(m):
			hit, failedAfter = m, nil
		}
	}
	switch {
	case failedAfter != nil:
		return finding{out: missing, why: fmt.Sprintf("#%d (%s) names %s after the change and failed", failedAfter.seq, mutationLabel(*failedAfter), p)}
	case hit != nil:
		return finding{out: matched, what: fmt.Sprintf("a change to %s at #%d", p, hit.seq), seq: hit.seq}
	}
	return finding{out: missing, why: "no call this session made is read as a change to " + p}
}

// markToken marks code a call showed, as the literal check keys it.
func markToken(atLine map[string]bool, t string) {
	atLine[normText(t)] = true
	lit := normText(strings.Trim(t, ",;:!?()[]{}'\""))
	if !strings.HasSuffix(lit, "...") {
		lit = strings.TrimRight(lit, ".")
	}
	atLine[lit] = true
}

// adviceTailRe is a condition or a purpose that makes an imperative advice.
var adviceTailRe = regexp.MustCompile(`\b(when|once|if|before|after|later|whenever|next time|as needed|when you're ready)\b|\bto (?:apply|pick up|take effect|confirm|verify|check|see|mention|make sure|keep|avoid|test|run|reflect|document|match|cover|get|try|ship|release|publish|review|deploy)\b`)

// unshownVerbRe are imperatives no change call shows: they are advice
// unless a command of that kind ran.
var unshownVerbRe = regexp.MustCompile(`^\W*(restart|install|deploy|commit|push|apply|configure|open|merge|publish|release|tag|review|run|test|document)\b`)

// ranWord reports a command this session ran that holds the word.
func (ix *recordIndex) ranWord(w string) bool {
	for _, o := range ix.obs {
		if (o.tool == "bash" || o.tool == "ssh") && o.usable() && prepareEvidence(o.command, false).hasLiteral(w) {
			return true
		}
	}
	return false
}

// changedFiles are the files the agent's successful changes named.
func (ix *recordIndex) changedFiles() []string {
	var out []string
	for _, m := range ix.mutations {
		if !m.person && !m.failed && m.known {
			out = append(out, m.targets...)
		}
	}
	return out
}

// changedAny reports a change of the agent's in this session that succeeded.
func (ix *recordIndex) changedAny() bool {
	for _, m := range ix.mutations {
		if !m.person && !m.failed {
			return true
		}
	}
	return false
}

// namedByChange says which call of the agent's, failed or not, may have
// changed p or a folder holding it: one naming it, or, for a path in the
// workspace, one whose files are not known; "" for none.
func (ix *recordIndex) namedByChange(p string) string {
	want := ix.inWorkspace(p)
	if want == "" {
		return ""
	}
	inside := ix.workDir != "" && strings.HasPrefix(want+"/", filepath.Clean(ix.workDir)+"/")
	for _, m := range ix.mutations {
		if m.person {
			continue
		}
		// A call whose files are not known may have changed any of the
		// workspace's (python fix.py); a remote one, none of them.
		if !m.known && m.tool != "ssh" && inside {
			return fmt.Sprintf("#%d (%s) may have changed any file in the workspace, %s among them", m.seq, mutationLabel(m), p)
		}
		for _, t := range append(append([]string(nil), m.targets...), m.covers...) {
			if t == "" {
				continue
			}
			t = filepath.Clean(t)
			if t == want || strings.HasPrefix(want, t+"/") || strings.HasPrefix(t, want+"/") {
				return fmt.Sprintf("%s was changed in this session (#%d)", p, m.seq)
			}
		}
	}
	return ""
}

// inWorkspace is where a path a claim names is: as written when absolute,
// else in the workspace; "" when that cannot be told.
func (ix *recordIndex) inWorkspace(p string) string {
	q := slashes(p)
	if !filepath.IsAbs(q) {
		if ix.workDir == "" {
			return ""
		}
		q = filepath.Join(ix.workDir, strings.TrimPrefix(filepath.Clean(q), "./"))
	}
	return filepath.Clean(q)
}

// localChange reports a change made in this session's workspace by a call
// read in full: a file tool's, or a known writer's line run in this session
// (not over ssh, not in a subagent's worktree).
func (ix *recordIndex) localChange(m *mutation) bool {
	switch m.tool {
	case "write", "edit":
		if m.child == "" {
			return true
		}
		// A subagent's file is the workspace's only outside its worktrees.
		for _, t := range m.targets {
			if ix.workDir == "" || !strings.HasPrefix(t, filepath.Clean(ix.workDir)+"/") || strings.Contains(t, "/"+tools.WorktreesDir+"/") {
				return false
			}
		}
		return true
	case "bash":
		return m.child == "" && m.writer
	}
	return false
}

// negatedChange contradicts "I did not change X" from cited evidence: a
// cited write or edit of X that succeeded.
func (ix *recordIndex) negatedChange(pool []*obsRec, p string) finding {
	want := ix.inWorkspace(p)
	for _, o := range pool {
		if (o.tool == "write" || o.tool == "edit") && !o.isError && o.path != "" && want != "" && filepath.Clean(o.path) == want {
			return finding{out: conflicted, seq: o.seq, why: fmt.Sprintf("the cited #%d is %s of %s", o.seq, map[string]string{"write": "a write", "edit": "an edit"}[o.tool], p),
				conflict: &Conflict{Seq: o.seq, CallID: o.callID, Excerpt: tailLines(o.content)}}
		}
	}
	return finding{out: missing, why: "a claim that something was not done is not shown by what was"}
}

// ranCommand checks "I ran `cmd`" against the commands in the record.
func (ix *recordIndex) ranCommand(cmd string) finding {
	for _, o := range ix.obs {
		// A command run on a host over ssh is one the agent ran too.
		if (o.tool == "bash" || o.tool == "ssh") && o.usable() && ranIn(o.line, o.exit, cmd) {
			return finding{out: matched, what: fmt.Sprintf("`%s` ran at #%d", clipRunes(cmd, 40), o.seq), seq: o.seq}
		}
	}
	return finding{out: missing, why: "`" + clipRunes(cmd, 60) + "` is not among the commands this session ran, as written"}
}

// checkFileLine checks a path:line cite against the file as it is now, and,
// where the claim quotes something, that the line or its neighbours hold it.
func checkFileLine(env checkEnv, fl fileLine, literals []string) (finding, string) {
	if env.readFile == nil {
		return finding{}, ""
	}
	p := fl.path
	if !filepath.IsAbs(p) && env.workDir != "" {
		p = filepath.Join(env.workDir, p)
	}
	data, err := env.readFile(p)
	label := fmt.Sprintf("%s:%d", fl.path, fl.line)
	if err != nil {
		return finding{out: missing, why: label + " cites a file that cannot be read now: " + firstLine(err.Error()),
			conflict: &Conflict{Excerpt: fl.path + ": " + firstLine(err.Error())}}, ""
	}
	lines := strings.Split(string(data), "\n")
	if fl.line < 1 || fl.line > len(lines) {
		return finding{out: missing, why: fmt.Sprintf("%s cites line %d; the file has %d", label, fl.line, len(lines)),
			conflict: &Conflict{Excerpt: fmt.Sprintf("%s has %d lines", fl.path, len(lines))}}, ""
	}
	from, to := max(0, fl.line-3), min(len(lines), fl.line+2)
	window := prepareEvidence(strings.Join(lines[from:to], "\n"), false)
	for _, lit := range literals {
		if strings.Contains(lit, normText(fl.path)) || strings.Contains(normText(fl.path), lit) {
			continue
		}
		if window.hasLiteral(lit) {
			return finding{out: matched, what: fmt.Sprintf("%q at %s", lit, label)}, lit
		}
		return finding{out: missing, why: fmt.Sprintf("%s does not show %q now", label, clipRunes(lit, 60))}, lit
	}
	return finding{}, "" // the line exists; with nothing quoted there is nothing more to compare
}

func poolName(cited bool) string {
	if cited {
		return "the cited output"
	}
	return "any current tool result in this session"
}

func mutationLabel(m mutation) string {
	switch {
	case m.path != "":
		return m.tool + " " + filepath.Base(m.path)
	case m.command != "":
		return "`" + clipRunes(m.command, 40) + "`"
	}
	return m.tool
}

func seqList(seqs []int64) []string {
	out := make([]string, len(seqs))
	for i, s := range seqs {
		out[i] = "#" + itoa(s)
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func tailLines(s string) string {
	const n = 3
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return clipRunes(strings.Join(lines, " / "), 160)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var compilingTestRe = regexp.MustCompile(`\b(?:go|cargo)\s+(?:test|nextest)\b|\b(?:mvnw?|gradlew?)\b[^|;&]*\btest\b|\bdotnet\s+test\b`)

var runWarnRe = regexp.MustCompile(`(?im)^\s*(?:\[warn(?:ing)?\]|warning\b)|:\s*warning\b|\b[1-9]\d* warnings?\b`)

// targetShown: a claim of building for a platform, architecture or mode is
// shown only by a run whose command names it ("in CI" never is).
func targetShown(target, command string) bool {
	alias := map[string][]string{"mac": {"darwin", "macos"}, "macos": {"darwin", "macos"}, "darwin": {"darwin"}, "x86_64": {"amd64", "x86_64"}, "amd64": {"amd64", "x86_64"}, "aarch64": {"arm64", "aarch64"}, "arm64": {"arm64", "aarch64"}, "release": {"release"}, "ci": nil, "production": {"prod"}}
	words, ok := alias[target]
	if !ok {
		words = []string{target}
	}
	low := strings.ToLower(command)
	for _, w := range words {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

var lintProblemsRe = regexp.MustCompile(`\b[1-9]\d* (?:problems?|warnings?|issues?|errors?)\b`)
