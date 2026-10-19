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
			rec.command, rec.path = args.Command, args.Path
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
				m := mutation{seq: seq, pos: pos, tool: o.Tool, command: args.Command, person: r.person, failed: o.IsError || o.OutcomeUnknown || (o.ExitCode != nil && *o.ExitCode != 0)}
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
		o.ev = prepareEvidence(o.content)
	}
	return o.ev
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
	pooled   bool // matched in a tool result, not in the record's calls or the files
	bare     bool // a number with nothing to say what it counts
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
	v := checkClaim(env, ix, c)
	if v.Verdict == ClaimVerified && extractFacts(citeRe.ReplaceAllString(c.Text, " ")).negated {
		v.Verdict = ClaimUnverified
		v.Reason = "it says something did not happen or is not so, which a result showing the thing does not show"
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
		if !f.any() && c.Quote == "" && len(c.Evidence) == 0 {
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
	} else if f.changed {
		for _, p := range f.paths {
			fd := ix.changedPath(p)
			findings = append(findings, fd)
			if fd.out == matched {
				atLine[normText(p)] = true
			}
		}
	}
	for _, cmd := range f.ran {
		fd := ix.ranCommand(cmd)
		findings = append(findings, fd)
		if fd.out == matched {
			atLine[normText(cmd)] = true
		}
	}
	for _, lit := range f.literals {
		if !atLine[lit] {
			findings = append(findings, ix.literal(pool, lit, cited))
		}
	}
	for _, cf := range f.counted {
		findings = append(findings, ix.counted(pool, cf, cited))
	}
	keyedOK := map[string]bool{}
	for _, kf := range f.keyed {
		if fd, ok := ix.keyed(pool, kf); ok {
			findings = append(findings, fd)
			keyedOK[kf.n] = true
		}
	}
	for _, n := range f.numbers {
		if countedNumber(f.counted, n) || keyedOK[n] {
			continue // judged with its unit above
		}
		findings = append(findings, ix.number(pool, n, cited))
	}
	if f.success {
		findings = append(findings, ix.success(pool, f.testish, f.scope, cited))
	}
	return findings
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

// citedMiss is what the claim says that its cited, current output does not
// hold: the record the agent pointed at says otherwise.
func citedMiss(pool []*obsRec, what string) finding {
	o := pool[len(pool)-1]
	return finding{out: conflicted, seq: o.seq, why: fmt.Sprintf("%s is not in the cited #%d", what, o.seq),
		conflict: &Conflict{Seq: o.seq, CallID: o.callID, Excerpt: clipRunes(strings.TrimSpace(o.content), 160)}}
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

// counted checks a number with the word it counts. Output that counts the
// same thing with another number, and never with this one, contradicts it.
func (ix *recordIndex) counted(pool []*obsRec, cf countFact, cited bool) finding {
	label := cf.n + " " + cf.unit
	if cf.unit == "%" {
		label = cf.n + "%"
	}
	var other *obsRec
	var otherAt countAt
	for i := len(pool) - 1; i >= 0; i-- {
		e := pool[i].evidence()
		for _, at := range e.counts[cf.unit] {
			if at.n == cf.n {
				return finding{out: matched, what: label + " in #" + itoa(pool[i].seq), seq: pool[i].seq, pooled: true}
			}
			if other == nil {
				other, otherAt = pool[i], at
			}
		}
	}
	// Another count of the same thing contradicts the claim only in the
	// output the claim cites: uncited, it may count something else.
	if other != nil && cited {
		return finding{out: conflicted, seq: other.seq,
			why:      fmt.Sprintf("it says %s; the cited #%d says %s", label, other.seq, otherAt.excerpt),
			conflict: &Conflict{Seq: other.seq, CallID: other.callID, Excerpt: otherAt.excerpt}}
	}
	// The output does not count that thing: the bare number decides.
	return ix.number(pool, cf.n, cited)
}

// keyed checks "port 8080" where the output labels the word, as "port: 8080"
// does; ok is false when no result labels it, and the bare number decides.
func (ix *recordIndex) keyed(pool []*obsRec, kf countFact) (finding, bool) {
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
	return finding{out: conflicted, seq: other.seq,
		why:      fmt.Sprintf("it says %s %s; #%d says %s", kf.unit, kf.n, other.seq, otherAt.excerpt),
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
func (ix *recordIndex) success(pool []*obsRec, testish string, scope []string, cited bool) finding {
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
			commandKind(lf.final) != testish {
			return nil, false
		}
		return runScope(lf.final, lf.finalCwd)
	}
	if cited {
		// Contradicted by a cited run of the same kind and the same scope
		// that failed on its own: no runner around it, exit 1.
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
			return finding{out: conflicted, seq: o.seq, why: fmt.Sprintf("the cited #%d is a %s of %s", o.seq, o.tool, p),
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
	window := prepareEvidence(strings.Join(lines[from:to], "\n"))
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
