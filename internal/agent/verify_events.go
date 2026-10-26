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
	staticPhrases = regexp.MustCompile(`\b(?:is|are|be|being)\s+(?:defined|declared|called|named|used|based|typed|exported|embedded|ordered|sorted|keyed|scoped|bounded|nested|shared|required|expected|allowed|supported|meant|known)\b|\b(?:so-called|built-in|well-known|hard-coded|zero-based)\b`)
	// outputSaysRe is a tool's output said as such: "pytest reported 2
	// skipped" describes the output, which the checks compare it with.
	outputSaysRe = regexp.MustCompile(`\b(?:pytest|jest|vitest|mocha|go test|go vet|npm|pnpm|yarn|cargo|kubectl|git|gofmt|(?:the|its|this|that)\s+(?:\w+'s\s+)?(?:output|run|tool|command|log|logs|result|results|test run|tests|suite|build|linter|summary|read|search|grep|listing|file|job|pipeline|scan))\s+(?:reported|printed|showed|said|returned|gave|listed|logged|reads|read|ran|counted|found|collected|matched)\b`)
	// eventWordRe are other words that report something new or different.
	eventWordRe = regexp.MustCompile(`\b(now|new|newly|gets|gains|all set|per your request|as requested|going forward|from here on|thanks to me|no longer|any ?more|post-change|pre-change|change|changes|edit|edits|diff|diffs|patch|patches|fix|fixes|commit|commits|pr|refactor|hotfix|update|updates)\b`)
	// countOutcomeRe is a count with its outcome word, which the count check
	// compares with the output's: "3 tests failed", "3 files changed".
	countOutcomeRe = regexp.MustCompile(`\b\d[\d,.]*\s+(?:[a-z\-]+\s+)?(?:passed|failed|skipped|deselected|errored|succeeded|changed|inserted|deleted|xfailed|xpassed)\b`)
	wordsRe        = regexp.MustCompile(`[a-z]+`)
	// observeRe is the agent saying it looked at something.
	observeRe = regexp.MustCompile(`(?:^|\b(?:i|we|i've|we've|i have|we have|the agent|abhed)\s+(?:\w+ly\s+|also\s+|then\s+|first\s+|just\s+)*)(read|opened|looked (?:at|through|into)|viewed|listed|searched|grepped|found|inspected|fetched|scanned|examined)\b`)
	// addRe and removeRe give a change's direction.
	addRe    = regexp.MustCompile(`\b(add|adds|added|adding|introduc\w*|insert\w*|new|newly|now)\b`)
	removeRe = regexp.MustCompile(`\b(remov\w*|delet\w*|drop\w*|strip\w*)\b`)
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
	// "after moving `total := 0` up": a change said as what came before.
	for _, m := range afterDoingRe.FindAllStringSubmatch(t, -1) {
		if changeActRe.MatchString(m[1]) {
			out = append(out, m[1])
		}
	}
	for _, w := range wordsRe.FindAllString(t, -1) {
		if irregularPast[w] || len(w) > 3 && strings.HasSuffix(w, "ed") && !notPastEd[w] {
			out = append(out, w)
		}
	}
	return out
}

// argMap is a call's string arguments by name.
func argMap(raw json.RawMessage) map[string]string {
	var m map[string]any
	out := map[string]string{}
	if json.Unmarshal(raw, &m) != nil {
		return out
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
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

// changeText is what one change did: the text it took away and the text it
// put in, and the file, for an edit, a write or `sed -i`; the command, for
// a command that changes a resource it names. fresh marks a write of a file
// the session had not read or written before, whose old text is unknown.
type changeText struct {
	seq      int64
	pos      float64
	path     string
	old, new string
	fresh    bool
	created  bool   // a fresh write that made the file: all of it is added
	full     string // a write's whole text, the next write's old text
}

// sedEdit reads `sed -i 's/old/new/' file`, any delimiter: what it takes
// away, what it puts in, and the file.
func sedEdit(cmd string) (old, new, file string, ok bool) {
	lf := readLine(cmd, "")
	if !lf.parsed || len(lf.cmds) != 1 || len(lf.cmds[0].words) < 4 || filepath.Base(lf.cmds[0].words[0]) != "sed" {
		return "", "", "", false
	}
	w := lf.cmds[0].words
	inPlace, expr := false, ""
	for i := 1; i < len(w)-1; i++ {
		switch {
		case w[i] == "-i" || strings.HasPrefix(w[i], "-i") || w[i] == "--in-place":
			inPlace = true
		case w[i] == "-e" && i+1 < len(w)-1:
			expr = w[i+1]
			i++
		case !strings.HasPrefix(w[i], "-") && expr == "":
			expr = w[i]
		}
	}
	if !inPlace || len(expr) < 4 || expr[0] != 's' {
		return "", "", "", false
	}
	parts := strings.Split(expr[2:], expr[1:2])
	if len(parts) < 2 {
		return "", "", "", false
	}
	return parts[0], parts[1], w[len(w)-1], true
}

// changeTexts are the session's changes that read as what they changed:
// edits and writes of files, `sed -i`, and the commands mutatingCmd takes.
// A note to memory, a commit message, a formatter or any other tool is
// none. A write's old text is what the session last read or wrote of the
// file before it; only the lines it changed are its text.
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
			out = append(out, changeText{seq: m.seq, pos: m.pos, path: a.Path, old: a.OldString, new: a.NewString})
		case "write":
			before, known := ix.before(a.Path, m.pos, out)
			if !known {
				out = append(out, changeText{seq: m.seq, pos: m.pos, path: a.Path, new: a.Content, fresh: true, full: a.Content, created: m.created})
				continue
			}
			oldL, newL := lineDiff(before, a.Content)
			out = append(out, changeText{seq: m.seq, pos: m.pos, path: a.Path, old: oldL, new: newL, full: a.Content})
		case "bash", "ssh":
			if o, n, file, ok := sedEdit(a.Command); ok && m.tool == "bash" {
				out = append(out, changeText{seq: m.seq, pos: m.pos, path: m.sedPath(file, ix), old: o, new: n})
			} else if mutatingCmd(a.Command) {
				out = append(out, changeText{seq: m.seq, pos: m.pos, new: a.Command})
			}
		}
	}
	return out
}

// sedPath is the file a `sed -i` line changed: its target, as the line was
// read, or the file named resolved in the workspace.
func (m mutation) sedPath(file string, ix *recordIndex) string {
	for _, t := range m.targets {
		if filepath.Base(t) == filepath.Base(file) {
			return t
		}
	}
	return ix.inWorkspace(file)
}

// before is what the session last read or wrote of path before pos.
func (ix *recordIndex) before(path string, pos float64, writes []changeText) (string, bool) {
	text, at, ok := "", -1.0, false
	for _, o := range ix.obs {
		if o.tool == "read" && !o.isError && o.pos < pos && o.pos > at && samePathAs(o.path, path) {
			text, at, ok = stripGutter(o.content), o.pos, true
		}
	}
	for _, w := range writes {
		if w.full != "" && w.pos < pos && w.pos > at && samePathAs(w.path, path) {
			text, at, ok = w.full, w.pos, true
		}
	}
	return text, ok
}

func samePathAs(a, b string) bool {
	return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b)
}

// lineDiff is the lines old has that new lacks, and the reverse.
func lineDiff(old, new string) (string, string) {
	in := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, l := range strings.Split(s, "\n") {
			m[strings.TrimSpace(l)] = true
		}
		return m
	}
	o, n := in(old), in(new)
	var gone, put []string
	for _, l := range strings.Split(old, "\n") {
		if t := strings.TrimSpace(l); t != "" && !n[t] {
			gone = append(gone, l)
		}
	}
	for _, l := range strings.Split(new, "\n") {
		if t := strings.TrimSpace(l); t != "" && !o[t] {
			put = append(put, l)
		}
	}
	return strings.Join(gone, "\n"), strings.Join(put, "\n")
}

// mutatingCmd reports a command that changes a resource it names: the verb
// is the first word after the program that is not a flag.
func mutatingCmd(cmd string) bool {
	f := strings.Fields(cmd)
	if len(f) < 2 {
		return false
	}
	verb := func(from int) (string, int) {
		for i := from; i < len(f); i++ {
			w := f[i]
			if strings.HasPrefix(w, "-") {
				if !strings.Contains(w, "=") && (w == "-n" || w == "--namespace" || w == "--context" || w == "--kubeconfig" || w == "-c" || w == "--cluster") {
					i++
				}
				continue
			}
			return w, i
		}
		return "", len(f)
	}
	switch filepath.Base(f[0]) {
	case "kubectl", "oc":
		v, i := verb(1)
		switch v {
		case "apply", "patch", "scale", "set", "edit", "delete", "create", "replace", "annotate", "label", "autoscale", "expose", "taint", "cordon", "uncordon", "drain":
			return true
		case "rollout":
			sub, _ := verb(i + 1)
			return sub == "restart" || sub == "undo" || sub == "resume" || sub == "pause"
		}
	case "helm":
		v, _ := verb(1)
		return v == "upgrade" || v == "install" || v == "uninstall" || v == "rollback"
	case "terraform", "tofu":
		v, _ := verb(1)
		return v == "apply" || v == "destroy" || v == "import"
	}
	return false
}

// changeNamed checks a change that names code against the session's
// changes, those of path only when path is given: one whose changed text
// holds every piece of code the claim names, the way it says. An addition
// needs one in what the change put in and not in what it took away; a
// removal, the reverse; any other change, one in either. A write of a file
// the session had not read says nothing of what it added or removed.
func (ix *recordIndex) changeNamed(f facts, path string) (finding, *evidenceText) {
	var tokens []string
	for _, t := range f.tokens {
		if !f.toolNamed[normText(t)] {
			tokens = append(tokens, t)
		}
	}
	if len(tokens) == 0 {
		return finding{out: missing, why: "it says what was changed but names neither a file nor code, and changes are read only from the calls"}, nil
	}
	cts := ix.changeTexts()
	for i := len(cts) - 1; i >= 0; i-- {
		ct := cts[i]
		if path != "" && !samePathAs(ct.path, path) {
			continue
		}
		if ct.fresh && !ct.created && f.dir != "" {
			continue
		}
		// A test file shows an addition only to a claim about tests.
		if ct.created && isTestFile(ct.path) && !f.saysTest {
			continue
		}
		oldE, newE := prepareEvidence(codeLines(ct.old, ct.path), false), prepareEvidence(codeLines(ct.new, ct.path), false)
		// The place a change was made: the changed text and the lines that
		// enclose it in the file as it stands after the change.
		var placeE *evidenceText
		after := func(nt string) bool {
			if placeE == nil {
				placeE = prepareEvidence(codeLines(enclosing(ix.fileAfter(ct), ct.new, ct.old), ct.path), false)
			}
			return placeE.hasLiteral(nt)
		}
		isFile := func(nt string) bool {
			return ct.path != "" && (nt == normText(filepath.Base(ct.path)) || nt == normText(ct.path) || strings.HasSuffix(normText(ct.path), "/"+nt))
		}
		// The token that shows the change: put in for an addition or a change
		// with no direction, taken away for a removal.
		hit, added := "", false
		for _, t := range tokens {
			nt := normText(t)
			inOld, inNew := oldE.hasLiteral(nt), newE.hasLiteral(nt)
			switch {
			case isFile(nt):
				if hit == "" && path == "" {
					hit = t // the changed file itself, named as code
				}
			case f.dir == "remove" && inOld && !inNew, f.dir != "remove" && inNew && (f.dir != "add" || !inOld):
				if hit == "" {
					hit, added = t, f.dir != ""
				}
			}
		}
		// The rest: in the changed text, what it says was replaced, or, once
		// something was added or removed, the place it was done in, in the
		// file as it stands after the change.
		ok := hit != ""
		for _, t := range tokens {
			nt := normText(t)
			inOld, inNew := oldE.hasLiteral(nt), newE.hasLiteral(nt)
			switch {
			case t == hit || isFile(nt) || inNew:
			case inOld && (f.oldNamed[nt] || f.dir == "remove"):
			case (added || f.inNamed[nt]) && !inOld && after(nt):
			default:
				ok = false
			}
		}
		if ok && f.onOff != "" && !onOffShown(f.onOff, newE) {
			ok = false
		}
		if ok {
			return finding{out: matched, call: true, what: fmt.Sprintf("a change given %q at #%d", clipRunes(hit, 40), ct.seq), seq: ct.seq}, newE
		}
	}
	where := "no edit, write or resource command this session"
	if path != "" {
		where = "no change this session to " + path
	}
	return finding{out: missing, why: "it says what was changed, and " + where + " changed " + fmt.Sprintf("%q", clipRunes(tokens[0], 40)) + " the way it says"}, nil
}

// isTestFile reports a file of tests.
func isTestFile(p string) bool {
	b := strings.ToLower(filepath.Base(p))
	return strings.HasSuffix(b, "_test.go") || strings.HasPrefix(b, "test_") || strings.HasSuffix(b, "_test.py") ||
		strings.Contains(b, ".spec.") || strings.Contains(b, ".test.") || strings.Contains(filepath.ToSlash(p), "/tests/")
}

// onOffShown reports a change that put in the value "enabled" or
// "disabled" says.
func onOffShown(want string, newE *evidenceText) bool {
	words := []string{"false", "off", "no", "0", "disabled", "disable"}
	if want == "on" {
		words = []string{"true", "on", "yes", "1", "enabled", "enable"}
	}
	for _, w := range words {
		if newE.hasLiteral(w) {
			return true
		}
	}
	return false
}

// codeLines drops the comment lines of a change's text, by the file's
// language: a comment that names code adds none.
func codeLines(text, path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	base := strings.ToLower(filepath.Base(path))
	hash := map[string]bool{".sh": true, ".bash": true, ".zsh": true, ".py": true, ".rb": true, ".yaml": true, ".yml": true,
		".toml": true, ".env": true, ".conf": true, ".cfg": true, ".pl": true, ".r": true, ".tf": true, ".ps1": true}[ext] ||
		base == "makefile" || base == "dockerfile" || strings.HasPrefix(base, ".env")
	slashes := map[string]bool{".go": true, ".js": true, ".mjs": true, ".ts": true, ".tsx": true, ".jsx": true, ".java": true,
		".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".rs": true, ".swift": true, ".kt": true, ".cs": true,
		".php": true, ".scala": true, ".css": true, ".scss": true, ".dart": true, ".proto": true}[ext] || ext == ""
	dashes := map[string]bool{".sql": true, ".lua": true, ".hs": true}[ext]
	var keep []string
	block := false
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case block:
			if strings.Contains(t, "*/") {
				block = false
			}
			continue
		case slashes && strings.HasPrefix(t, "/*"):
			block = !strings.Contains(t, "*/")
			continue
		case slashes && strings.HasPrefix(t, "//"), hash && strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "#!"), dashes && strings.HasPrefix(t, "--"):
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

// enclosing is a change's text with the lines that enclose it in the file
// after the change: from where its text sits, each earlier line indented
// less than the last one taken (the loop, then the function, around a
// line). With the file or the change's place unknown, the change's text.
func enclosing(file, new, old string) string {
	hunk := new + "\n" + old
	lines := strings.Split(file, "\n")
	at := -1
	for _, l := range strings.Split(new, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			for i, fl := range lines {
				if strings.TrimSpace(fl) == t {
					at = i
					break
				}
			}
			break
		}
	}
	if at < 0 {
		return hunk
	}
	out := []string{hunk}
	depth := indentOf(lines[at])
	for i := at - 1; i >= 0 && depth > 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if d := indentOf(lines[i]); d < depth {
			out = append(out, lines[i])
			depth = d
		}
	}
	return strings.Join(out, "\n")
}

// indentOf is a line's leading white space, a tab as four spaces.
func indentOf(l string) int {
	n := 0
	for _, r := range l {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// fileAfter is a changed file as it stood after the change: a write's text,
// a current read, or an earlier read with an edit's text put in.
func (ix *recordIndex) fileAfter(ct changeText) string {
	if ct.full != "" {
		return ct.full
	}
	var cur, earlier *obsRec
	for _, o := range ix.obs {
		if o.tool != "read" || o.isError || !samePathAs(o.path, ct.path) {
			continue
		}
		switch {
		case o.pos > ct.pos && ix.staleBy(o) == nil:
			cur = o
		case o.pos < ct.pos:
			earlier = o
		}
	}
	switch {
	case cur != nil:
		return stripGutter(cur.content)
	case earlier != nil && ct.old != "":
		return strings.Replace(stripGutter(earlier.content), ct.old, ct.new, 1)
	}
	return ""
}

// gutterRe is the read tool's line number before each line.
var gutterRe = regexp.MustCompile(`(?m)^ *\d+\t`)

// stripGutter takes the read tool's line numbers off a read's text, when
// every line has one.
func stripGutter(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for _, l := range lines {
		if l != "" && !gutterRe.MatchString(l) {
			return s
		}
	}
	return gutterRe.ReplaceAllString(s, "")
}

// observed checks what the agent says it looked at against its reads,
// listings and searches: every path it names by a call of its own (the
// path itself, or one ending in it at a slash), or, for "found … in Y", a
// search whose result names Y; every piece of code by a search for it
// ("searched for `API_KEY`"), or by a read or search whose result holds it.
func (ix *recordIndex) observed(f facts) finding {
	search := f.searched
	var cur []*obsRec
	for _, o := range ix.obs {
		if o.usable() && !o.isError && shownTools[o.tool] && o.child == "" {
			cur = append(cur, o)
		}
	}
	isSearch := func(o *obsRec) bool { return o.tool == "grep" || o.tool == "glob" || o.tool == "web_search" }
	pathNamed := func(o *obsRec, p string) bool {
		for _, a := range []string{o.path, o.argMap["path"], o.argMap["dir"], o.argMap["root"]} {
			if a != "" && ix.pathIs(a, p) {
				return true
			}
		}
		return false
	}
	// "across the whole repo" needs a search of the whole workspace.
	if f.repoWide && search {
		whole := false
		for _, o := range cur {
			if isSearch(o) {
				root := o.argMap["path"]
				whole = whole || root == "" || root == "." || ix.workDir != "" && filepath.Clean(root) == filepath.Clean(ix.workDir)
			}
		}
		if !whole {
			return finding{out: missing, why: "it says the whole workspace was searched, and no search this session was of all of it"}
		}
	}
	isPathTok := map[string]bool{}
	for _, p := range f.paths {
		isPathTok[normText(p)] = true
		ok := false
		for _, o := range cur {
			if pathNamed(o, p) || f.found && isSearch(o) && o.evidence().hasLiteral(normText(p)) {
				ok = true
				break
			}
		}
		if !ok {
			return finding{out: missing, why: "it says " + p + " was looked at, and no read, listing or search this session names it"}
		}
	}
	for _, t := range f.tokens {
		nt := normText(t)
		if isPathTok[nt] || isPathTok[normText(rawPath(t))] {
			continue
		}
		ok := false
		for _, o := range cur {
			pattern := strings.ToLower(o.argMap["pattern"] + " " + o.argMap["query"])
			switch {
			case search && isSearch(o) && prepareEvidence(pattern, false).hasLiteral(nt):
				ok = true
			case !search && o.evidence().hasLiteral(nt):
				// "I read `calc`" names no file: a read is of a file. Code
				// is found in the file it names, or by a search.
				// A code name the read file declares is the file's: "I read
				// the `Sum` function".
				inPath := f.found && isSearch(o) || len(f.paths) == 0 && o.tool == "read" && declares(o.content, t)
				for _, p := range f.paths {
					inPath = inPath || pathNamed(o, p) || isSearch(o)
				}
				ok = inPath
			}
			if ok {
				break
			}
		}
		if !ok {
			return finding{out: missing, why: fmt.Sprintf("it says %q was looked at or found, and no read or search this session shows it", clipRunes(t, 40))}
		}
	}
	return finding{out: matched, call: true, what: "the reads and searches it names"}
}

// declares reports a declaration of name in a file's text.
func declares(content, name string) bool {
	re, err := regexp.Compile(`(?m)\b(?:func|def|class|function|type|struct|fn|interface|enum|trait|const|var|let)\s+(?:\([^)]*\)\s*)?` + regexp.QuoteMeta(name) + `\b`)
	return err == nil && re.MatchString(content)
}

// pathIs reports a call's path a that is the path p a claim names: the same
// file, or one ending in p at a slash.
func (ix *recordIndex) pathIs(a, p string) bool {
	ca, cp := filepath.Clean(slashes(a)), filepath.Clean(slashes(p))
	if ca == cp || ca == ix.inWorkspace(p) {
		return true
	}
	return !filepath.IsAbs(cp) && strings.HasSuffix(ca, "/"+cp)
}

// changeActRe are verbs of changing code or a resource, in any form.
var changeActRe = regexp.MustCompile(`\b(add\w*|remov\w*|updat\w*|chang\w*|rewr\w*|mov\w*|renam\w*|replac\w*|insert\w*|delet\w*|refactor\w*|fix\w*|set|sets|setting|edit\w*|modif\w*|introduc\w*|implement\w*|simplif\w*|wir\w*|put|puts|turn\w*|scal\w*|scrub\w*|drop\w*|strip\w*|patch\w*|bump\w*|upgrad\w*|downgrad\w*|revert\w*|enabl\w*|disabl\w*|swap\w*|convert\w*|extract\w*|inlin\w*|merg\w*|split\w*|clean\w*|creat\w*|wrote|written|writ\w*|gets|gains|tweak\w*|adjust\w*|reorder\w*|restructur\w*|migrat\w*|port\w*|hardcod\w*|hard-cod\w*)\b`)

// observeChainRe is a further look chained to the first: "and found it".
var observeChainRe = regexp.MustCompile(`(?:\band|\bthen|,)\s+(?:then\s+)?(?:read|opened|looked (?:at|through|into)|viewed|listed|searched(?: for)?|grepped(?: for)?|found|inspected|fetched|scanned|examined)\b`)

// afterDoingRe is a change said as what came before, leading a sentence:
// "After moving `total := 0` up, ...".
var afterDoingRe = regexp.MustCompile(`^\W*(?:after|since|by|from|once|upon|with)\s+(?:\w+ly\s+)?(\w+ing)\b`)

// bareDeedRe is a verb that is the same in the present and the past, after
// "and" or "then": "I opened X and put Y there".
var bareDeedRe = regexp.MustCompile(`\b(?:and|then)\s+(?:\w+ly\s+)?(set|put|cut|let|split|shut|reset|hit|quit)\b`)

// passWords are events a run shows.
var passWords = wordSet(`passed failed succeeded ran executed errored skipped deselected timed`)

// eventKinds are the kinds of call a claim's events need: "run" for a pass
// or a run, "look" for what was looked at, "change" for any other.
func eventKinds(text string, f facts) []string {
	if f.event == "" {
		return nil
	}
	need := map[string]bool{}
	low := normText(backtickRe.ReplaceAllString(text, " "))
	rest := low
	if f.observe {
		need["look"] = true
		rest = observeChainRe.ReplaceAllString(observeRe.ReplaceAllString(low, " "), " ")
		if bareDeedRe.MatchString(rest) {
			need["change"] = true
		}
	}
	for _, w := range eventWords(rest) {
		switch {
		case passWords[w] && aboutRuns(low, f) || f.success && w == "done":
			need["run"] = true
		case w == "now" && f.success && !f.changeAct && !strings.Contains(low, "now that"):
			need["run"] = true // "the tests pass now": a time of the run
		case f.observe && (w == "found" || w == "read"):
		default:
			need["change"] = true
		}
	}
	if (f.deed || f.changed) && !f.observeOnly && len(f.ran) == 0 && !f.success {
		need["change"] = true
	}
	var out []string
	for _, k := range []string{"run", "look", "change"} {
		if need[k] {
			out = append(out, k)
		}
	}
	return out
}

// aboutRuns reports a sentence about tests, a build or a command, where
// "skipped" or "failed" is a run's.
func aboutRuns(low string, f facts) bool {
	return f.testish != "" || len(f.ran) > 0 || testRe.MatchString(low) || runnerRe.MatchString(low)
}

var runnerRe = regexp.MustCompile(`\b(pytest|jest|vitest|go test|go vet|npm test|cargo test|make|build|lint|linter|command|script|job|pipeline|ci)\b`)

// searchVerbRe says the agent searched for something.
var searchVerbRe = regexp.MustCompile(`\b(searched|grepped|looked for|searched for|grepped for)\b`)

// clauseSepRe are where a sentence may join clauses.
var clauseSepRe = regexp.MustCompile(`\s*(?:,|;|:|—|\s-\s)\s*(?:and\s+|then\s+|and then\s+)?|\s+(?:and then|and|then)\s+`)

// verbRe marks a piece of a sentence that has a verb of its own.
var verbRe = regexp.MustCompile(`\b(?:is|are|was|were|be|been|has|have|had|do|does|did|can|will|would|should|may|might|must|i|we|it|they|returns?|adds?|skips?|uses?|runs?|calls?|holds?|keeps?|starts?|makes?|takes?|gives?|sets?|puts?|reads?|writes?|listens?|stores?|loops?|checks?|handles?|works?|pass|passes|fails?|exits?|prints?|shows?|lists?|points?|means?|needs?|lets?|sees?|stays?|remains?|contains?|looks?|goes|went|comes?|gets?|gains?)\b`)

// clauses cuts a claim's text at ",", ";", ":", "—", "and" and "then" where
// each side has a verb of its own; a piece with none stays with the one
// before it, and a look chained to a look ("searched for X and found it")
// stays one clause. Code spans are never cut.
func clauses(text string) []string {
	var spans []string
	masked := backtickRe.ReplaceAllStringFunc(text, func(m string) string {
		spans = append(spans, m)
		return fmt.Sprintf("\x00%d\x00", len(spans)-1)
	})
	unmask := func(s string) string {
		for i, sp := range spans {
			s = strings.ReplaceAll(s, fmt.Sprintf("\x00%d\x00", i), sp)
		}
		return strings.TrimSpace(s)
	}
	var out []string
	last, held := 0, ""
	cut := func(end, next int) {
		piece := unmask(masked[last:end])
		if held != "" {
			piece, held = held+" "+piece, ""
		}
		low := normText(backtickRe.ReplaceAllString(piece, " "))
		has := verbRe.MatchString(low) || len(eventWords(piece)) > 0 || observeRe.MatchString(low) || successRe.MatchString(low)
		look := observeRe.MatchString(low) && len(eventWords(observeChainRe.ReplaceAllString(observeRe.ReplaceAllString(low, " "), " "))) == 0
		switch {
		case piece == "":
		case len(out) == 0 && !has:
			held = piece // a lead with no verb goes with what follows
		case len(out) > 0 && !has:
			out[len(out)-1] += " " + piece
		case len(out) > 0 && look && lookOnly(out[len(out)-1]):
			out[len(out)-1] += " and " + piece
		default:
			out = append(out, piece)
		}
		last = next
	}
	for _, m := range clauseSepRe.FindAllStringIndex(masked, -1) {
		cut(m[0], m[1])
	}
	cut(len(masked), len(masked))
	if held != "" {
		out = append(out, held)
	}
	return out
}

// lookOnly reports a clause that only says what was looked at.
func lookOnly(s string) bool {
	low := normText(backtickRe.ReplaceAllString(s, " "))
	return observeRe.MatchString(low) && len(eventWords(observeRe.ReplaceAllString(low, " "))) == 0
}
