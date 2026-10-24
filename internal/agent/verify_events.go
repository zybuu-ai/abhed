package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Output shows content as it stands: a read, a run's text. A sentence that
// reports an event (something done, changed, made new, or found) says what
// happened, which only the calls show. eventWord finds what makes a sentence
// an event, whatever its wording or person: a verb in the past or a past
// participle (any word in -ed, or an irregular one), "now", "new", "no
// longer", "any more", or a change named as a thing. Code spans are left
// out, and so are a count's own outcome ("3 tests failed"), which the count
// check compares with the output's.

var (
	// irregularPast are past forms and participles that are not also the
	// present ("set", "put", "read" and "cut" are, and are left to the
	// first-person deed reading).
	irregularPast = wordSet(`arose awoke borne born beaten became begun bent bit bitten bled blew blown broke broken
		bred brought built burnt bought caught chose chosen clung came crept dealt dug did done drew drawn drank
		drunk drove driven ate eaten fell fallen fed felt fought found fled flung flew flown forbade forbidden
		forgot forgotten forgave forgiven froze frozen got gotten gave given went gone ground grew grown hung heard
		hid hidden held kept knelt knew known laid led left lent lain lit lost made meant met paid proven rode
		ridden rang rung rose risen ran sold sent sewn shook shaken shone shot shown shrank shrunk sang sung sank
		sunk sat slept slid spoke spoken spent spun spat sprang sprung stood stole stolen stuck stung stank struck
		strove swore sworn swept swam swum swung took taken taught tore torn told thought threw thrown understood
		undid undone woke woken wore worn wove woven wept won wound wrote written rewrote rewritten rebuilt redone
		reran rerun withdrew withdrawn overrode overridden overwrote overwritten`)
	// notPastEd are words in -ed that are no past form.
	notPastEd = wordSet(`need feed speed seed bleed breed proceed succeed exceed embed shed bed red hundred sacred
		kindred naked wicked rugged ragged beloved indeed weed reed steed greed creed deed heed shred sled wed bred
		fled led fed`)
	// staticPhrases are participles that describe code as it stands, not an
	// event: the short allowlist the guide names.
	staticPhrases = regexp.MustCompile(`\b(?:is|are|be|being)\s+(?:defined|declared|called|named|used|based|typed|exported|embedded|guarded|protected|locked|ordered|sorted|keyed|scoped|bounded|nested|shared|required|expected|allowed|supported|meant|known)\b|\b(?:so-called|built-in|well-known|hard-coded|zero-based)\b`)
	// outputSaysRe is a tool's output said as such: "pytest reported 2
	// skipped" describes the output, which the checks compare it with.
	outputSaysRe = regexp.MustCompile(`\b(?:pytest|jest|vitest|mocha|go test|go vet|npm|pnpm|yarn|cargo|kubectl|git|gofmt|(?:the|its|this|that)\s+(?:\w+'s\s+)?(?:output|run|tool|command|log|logs|result|results|test run|tests|suite|build|linter|summary|read|search|grep|listing|file|job|pipeline|scan))\s+(?:reported|printed|showed|said|returned|gave|listed|logged|reads|read|ran|counted|found|collected|matched)\b`)
	// eventWordRe are other words that report something new or different.
	eventWordRe = regexp.MustCompile(`\b(now|new|newly|no longer|any ?more|post-change|pre-change|change|changes|edit|edits|diff|diffs|patch|patches|fix|fixes|commit|commits|pr|refactor|hotfix|update|updates|done)\b`)
	// countOutcomeRe is a count with its outcome word, which the count check
	// compares with the output's: "3 tests failed", "3 files changed".
	countOutcomeRe = regexp.MustCompile(`\b\d[\d,.]*\s+(?:[a-z\-]+\s+)?(?:passed|failed|skipped|deselected|errored|succeeded|changed|inserted|deleted|xfailed|xpassed)\b`)
	wordsRe        = regexp.MustCompile(`[a-z]+`)
	// observeRe is the agent saying it looked at something.
	observeRe = regexp.MustCompile(`(?:^|\b(?:i|we|i've|we've|i have|we have|the agent|abhed)\s+(?:\w+ly\s+|also\s+|then\s+|first\s+|just\s+)*)(read|opened|looked (?:at|through|into)|viewed|listed|searched|grepped|found|inspected|fetched|scanned|examined)\b`)
	// addRe and removeRe give a change's direction.
	addRe    = regexp.MustCompile(`\b(add|adds|added|adding|introduc\w*|insert\w*|new|newly)\b`)
	removeRe = regexp.MustCompile(`\b(remov\w*|delet\w*|drop\w*|strip\w*)\b`)
	// mutatingCmdRe are commands that change a resource they name.
	mutatingCmdRe = regexp.MustCompile(`^\s*(?:kubectl\s+(?:\S+\s+)*?(?:apply|patch|scale|set|edit|delete|create|replace|annotate|label|rollout\s+(?:restart|undo))|helm\s+(?:upgrade|install|uninstall|rollback)|terraform\s+(?:apply|destroy|import))\b`)
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// eventWord is the first word that makes a claim's text an event, or "".
func eventWord(text string) string {
	if ws := eventWords(text); len(ws) > 0 {
		return ws[0]
	}
	return ""
}

// eventWords are every word that makes a claim's text an event.
func eventWords(text string) []string {
	t := normText(backtickRe.ReplaceAllString(text, " "))
	t = countOutcomeRe.ReplaceAllString(t, " ")
	t = staticPhrases.ReplaceAllString(t, " ")
	t = outputSaysRe.ReplaceAllString(t, " ")
	t = noFailRe.ReplaceAllString(t, " ")
	out := eventWordRe.FindAllString(t, -1)
	for _, w := range wordsRe.FindAllString(t, -1) {
		if irregularPast[w] || len(w) > 3 && strings.HasSuffix(w, "ed") && !notPastEd[w] {
			out = append(out, w)
		}
	}
	return out
}

// argText is a call's arguments as text: every string value, decoded.
func argText(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var parts []string
	for _, v := range m {
		if s, ok := v.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// changeText is what one change was given: the text it took away and the
// text it put in, and the file, for an edit or a write; the command, for a
// command that changes a resource it names.
type changeText struct {
	seq      int64
	path     string
	old, new string
}

// changeTexts are the session's changes that read as what they changed:
// edits and writes of files, and the commands in mutatingCmdRe. A note to
// memory, a commit message, a formatter or any other tool is none.
func (ix *recordIndex) changeTexts() []changeText {
	var out []changeText
	for _, m := range ix.mutations {
		if m.person || m.failed || m.text == "" {
			continue
		}
		var a struct {
			Path      string `json:"path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
			Content   string `json:"content"`
			Command   string `json:"command"`
		}
		if json.Unmarshal([]byte(m.text), &a) != nil {
			continue
		}
		switch m.tool {
		case "edit":
			out = append(out, changeText{seq: m.seq, path: a.Path, old: a.OldString, new: a.NewString})
		case "write":
			out = append(out, changeText{seq: m.seq, path: a.Path, new: a.Content})
		case "bash", "ssh":
			if mutatingCmdRe.MatchString(a.Command) {
				out = append(out, changeText{seq: m.seq, new: a.Command})
			}
		}
	}
	return out
}

// changeNamed checks a change that names no file against the session's
// changes: one whose text holds what the claim names, the way it says. An
// addition needs a token in what the change put in and not in what it took
// away; a removal, the reverse; any other change, a token in either. Every
// token it names must be in the change or in a current read of the changed
// file.
func (ix *recordIndex) changeNamed(f facts) (finding, string, *evidenceText) {
	tokens := f.tokens
	if len(tokens) == 0 {
		return finding{out: missing, why: "it says what was changed but names neither a file nor code, and changes are read only from the calls"}, "", nil
	}
	cts := ix.changeTexts()
	for i := len(cts) - 1; i >= 0; i-- {
		ct := cts[i]
		oldE, newE := prepareEvidence(ct.old, false), prepareEvidence(ct.new, false)
		hit, all := "", true
		for _, t := range tokens {
			nt := normText(t)
			inOld, inNew := oldE.hasLiteral(nt), newE.hasLiteral(nt)
			switch {
			case ct.path != "" && nt == normText(filepath.Base(ct.path)):
				if hit == "" {
					hit = t // the changed file itself, named as code
				}
			case f.dir == "add" && inNew && !inOld, f.dir == "remove" && inOld && !inNew, f.dir == "" && (inNew || inOld):
				if hit == "" {
					hit = t
				}
			case inOld || inNew:
			case !ix.readHolds(ct.path, nt):
				all = false
			}
		}
		if hit != "" && all {
			return finding{out: matched, call: true, what: fmt.Sprintf("a change given %q at #%d", clipRunes(hit, 40), ct.seq), seq: ct.seq}, hit, newE
		}
	}
	return finding{out: missing, why: "it says what was changed but names no file, and no edit, write or resource command this session was given " + fmt.Sprintf("%q", clipRunes(tokens[0], 40)) + " the way it says"}, "", nil
}

// readHolds reports a current read of path that holds the token.
func (ix *recordIndex) readHolds(path, tok string) bool {
	if path == "" {
		return false
	}
	for _, o := range ix.obs {
		if o.tool == "read" && o.usable() && !o.isError && filepath.Clean(o.path) == filepath.Clean(path) &&
			ix.staleBy(o) == nil && o.evidence().hasLiteral(tok) {
			return true
		}
	}
	return false
}

// observed checks "I read `sum.go`", "I searched for `API_KEY`", "I found
// `API_KEY` in `config/app.env`" against the reads and searches: one whose
// arguments name what the claim names and, for found, whose result holds
// the code it says was found.
func (ix *recordIndex) observed(f facts) finding {
	names := append(append([]string(nil), f.paths...), f.tokens...)
	for i := len(ix.obs) - 1; i >= 0; i-- {
		o := ix.obs[i]
		if !o.usable() || o.isError || !shownTools[o.tool] || o.child != "" {
			continue
		}
		args := strings.ToLower(o.args)
		named, found := false, !f.found
		for _, n := range names {
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "" {
				continue
			}
			if strings.Contains(args, n) || o.path != "" && filepath.Base(o.path) == filepath.Base(n) {
				named = true
			} else if f.found && o.evidence().hasLiteral(normText(n)) {
				found = true
			}
		}
		if named && found {
			return finding{out: matched, call: true, what: fmt.Sprintf("the %s at #%d", o.tool, o.seq), seq: o.seq}
		}
	}
	return finding{out: missing, why: "it says what was looked at, and no read or search this session names it"}
}
