package agent

import (
	"path/filepath"
	"regexp"
	"strings"
)

// An answer that explains what a result showed (a function walked through
// line by line, a definition, an analogy, the reason a line is there) makes
// sentences that are no claim about the session's work. Left as statements
// about the work, each one the checks cannot match counts as unverified work.
// explains picks out those sentences, conservatively: anything that says
// what was done, what ran, what passed, a count or a state of a system stays
// a claim about the work, and so does anything naming code the shown result
// does not hold.

var (
	// explainRe is how an explanation reads: an analogy, a definition, a
	// reason, or a walk through code.
	explainRe = regexp.MustCompile(`\b((?:here|this) is how|like|think of|imagine|as if|analogy|is a|is an|are a|means|refers to|stands for|because|since|so that|in order to|this is why|that is why|which is why|this (?:line|function|loop|variable|means|is)|here we|we need to|we (?:then|first|start|keep|add|check|loop|return|skip|ignore|move|write)|it (?:then|first|keeps|adds|checks|loops|returns|skips|starts|hands|gives|visits|walks))\b`)
	// defineRe is a definition or an analogy, the only explanation that may
	// name no code.
	defineRe = regexp.MustCompile(`\b(like|think of|imagine|as if|analogy|is a|is an|are a|means|refers to|stands for)\b`)
	// stateRe are words that say how a system or a run stands.
	stateRe = regexp.MustCompile(`\b(pass(?:es|ed|ing)?|fail(?:s|ed|ing|ure|ures)?|succeed(?:s|ed)?|errors?|errored|ready|(?:is|are|was|were|went|goes|go) (?:down|up)|healthy|unhealthy|available|unavailable|pending|broken|missing|outdated|degraded|crash(?:es|ed|ing)?|deployed|restarted)\b`)
	// outcomeRe are words that say something came about: an explanation
	// says how shown content is, not what changed.
	outcomeRe = regexp.MustCompile(`\b(now|no longer|any ?more|again|fixed|resolved|solved|addressed|works now|now works|is working|recovered|finished|complete|completed|(?:is|are|was|were) done|patched|landed|merged|live|shipped|released|corrected|reviewed|approved|rolled out|went out|cleared|gone|green|stable|succeeded|works again)\b`)
	// didRe is the agent saying what it did.
	didRe = regexp.MustCompile(`\b(?:i|we|i've|we've|i'd|we'd|i'm|we're)\s+(?:have\s+|had\s+|just\s+|also\s+|then\s+|now\s+|already\s+)?(?:deployed|restarted|installed|configured|enabled|disabled|started|stopped|killed|merged|pushed|released|rolled|scaled|migrated|ran|did|made|wrote|built|sent|verified|checked|tested|confirmed|fixed|cleaned|applied|upgraded|downgraded|reverted|rebooted|provisioned|created|deleted|removed|updated|changed|added|edited|modified|shipped|corrected|reviewed|approved|resolved|solved|patched|landed|done|finished|completed|kept|left|preserved|avoided)\b`)
	// workNegRe is a negation about a change, the work or how something
	// stands, which an explanation of shown code never needs; one about what
	// the code does ("the loop body never runs") is another matter.
	workNegRe = regexp.MustCompile(`\b(unchanged|untouched|unmodified|unaltered|left alone|intact|as before|preserved|original|no longer|any ?more|nothing\b[^.;]{0,40}\bchanged|(?:not|never|n't)\s+(?:been\s+|be\s+)?(?:\w+\s+)?(?:changed|modified|touched|edited|altered)|means nothing|does(?:n't| not) matter)\b`)
	// judgeRe are words that judge the work or a system, never needed to
	// explain code.
	judgeRe = regexp.MustCompile(`\b(issues?|problems?|bugs?|cause[sd]?|harmless|real|matters?|present|safe|secure|leaks?|outage|prod|production|stale|cached|regression|incident|vulnerab\w*)\b`)
	// codeTokenRe splits a code span into its words.
	codeTokenRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*|\d+`)
	// codePunctRe marks a span written as code, not prose in backticks.
	codePunctRe = regexp.MustCompile(`[()\[\]{}=:+*<>!&|%^.,;]|\b_\b`)
	// wordRe are the words of a sentence, for what a definition is about.
	wordRe = regexp.MustCompile(`[a-z]{4,}`)
)

// shownTools are the tools whose results show content an explanation may be
// about: reads and searches, never a run, a change or its report.
var shownTools = map[string]bool{"read": true, "grep": true, "glob": true, "ls": true, "web_fetch": true, "web_search": true}

// explains reports a claim, unverified by the checks, that explains shown
// content rather than saying anything about the session's work. It is an
// allowlist: anything that may say what was done, what came about, how
// something stands, or a judgement of the work stays a claim about it.
func explains(ix *recordIndex, c Claim) bool {
	text := citeRe.ReplaceAllString(c.Text, " ")
	f := extractFacts(text)
	low := normText(text)
	if f.success || f.changed || len(f.ran) > 0 || len(f.fileLines) > 0 || len(f.counted) > 0 ||
		len(f.hosts) > 0 || c.Quote != "" || didRe.MatchString(low) || stateRe.MatchString(low) ||
		outcomeRe.MatchString(low) || workNegRe.MatchString(low) || judgeRe.MatchString(low) ||
		!explainRe.MatchString(low) {
		return false
	}
	spans := codeSpans(c.Text)
	// What it explains must be shown by a read or a search: the results it
	// cites, every one of them such, or, citing none, the session's current
	// ones. A session with none has nothing shown to explain.
	var shown []*obsRec
	if len(c.Evidence) > 0 {
		for _, ref := range c.Evidence {
			o, _ := ix.resolve(ref)
			if o == nil || !shownTools[o.tool] || o.isError || ix.staleBy(o) != nil {
				return false
			}
			shown = append(shown, o)
		}
	} else {
		for _, o := range ix.obs {
			if o.usable() && shownTools[o.tool] && !o.isError && ix.staleBy(o) == nil {
				shown = append(shown, o)
			}
		}
	}
	if len(shown) == 0 {
		return false
	}
	// "port 8080" is a setting the result can label; "gives 0" is prose.
	for _, kf := range f.keyed {
		for _, o := range shown {
			if len(o.evidence().labels[kf.unit]) > 0 {
				return false
			}
		}
	}
	if len(spans) == 0 && len(f.numbers) == 0 && len(f.literals) == 0 {
		// No code in it: only a definition or an analogy, about something a
		// read or a search showed.
		return defineRe.MatchString(low) && aboutShown(low, shown)
	}
	for _, o := range shown {
		if holdsAll(o, spans, f) {
			return true
		}
	}
	return false
}

// aboutShown reports a sentence sharing a word of four letters or more with
// a shown result, compared in singular.
func aboutShown(low string, shown []*obsRec) bool {
	for _, w := range wordRe.FindAllString(low, -1) {
		if explainRe.MatchString(w) || notUnits[w] || qualStop[w] {
			continue
		}
		u := unitOf(w)
		for _, o := range shown {
			if o.evidence().hasLiteral(u) || o.evidence().hasLiteral(w) {
				return true
			}
		}
	}
	return false
}

// codeSpans are the backtick spans of a claim.
func codeSpans(text string) []string {
	var out []string
	for _, m := range backtickRe.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// holdsAll reports a result that shows every piece of code a claim names:
// each code span as written or, where it reads as code, word by word; every
// number; and every other literal (a path, a quoted phrase) as written.
func holdsAll(o *obsRec, spans []string, f facts) bool {
	e := o.evidence()
	// A read's own path is shown too: "how the cache in `cache.go` works".
	isPath := func(lit string) bool {
		return o.path != "" && (lit == normText(o.path) || lit == normText(filepath.Base(o.path)))
	}
	inSpan := map[string]bool{}
	for _, sp := range spans {
		inSpan[normText(sp)] = true
		if e.hasLiteral(normText(sp)) || isPath(normText(sp)) {
			continue
		}
		if !codePunctRe.MatchString(sp) && !strings.ContainsAny(sp, "_0123456789") && strings.ToLower(sp) == sp {
			return false // prose in backticks the result does not hold
		}
		for _, tok := range codeTokenRe.FindAllString(sp, -1) {
			if !e.hasLiteral(strings.ToLower(tok)) {
				return false
			}
		}
	}
	for _, lit := range f.literals {
		if !inSpan[lit] && !e.hasLiteral(lit) && !isPath(lit) {
			return false
		}
	}
	for _, n := range f.numbers {
		if !e.numbers[n] {
			return false
		}
	}
	return true
}

// unsaid is why a verified claim is not: it says the agent did something the
// calls do not show (only changes and commands are read from them), or it
// gives a state that none of the results it matched says.
func unsaid(ix *recordIndex, c Claim, v ClaimVerdict) string {
	text := citeRe.ReplaceAllString(c.Text, " ")
	f := extractFacts(text)
	if f.success {
		return ""
	}
	low := normText(text)
	if !f.changed && len(f.ran) == 0 && didRe.MatchString(low) {
		return "it says what was done, which is read only from the calls, and none is read as that"
	}
	for _, w := range append(stateRe.FindAllString(low, -1), outcomeRe.FindAllString(low, -1)...) {
		if w == "now" || w == "again" {
			continue // a time, not a thing an output says
		}
		w = w[strings.LastIndexByte(w, ' ')+1:]
		said := false
		for _, seq := range v.Evidence {
			if o := ix.bySeq[seq]; o != nil && o.evidence().hasLiteral(w) {
				said = true
			}
		}
		if !said {
			return "it says " + w + ", which no result it matched says"
		}
	}
	return ""
}
