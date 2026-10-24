package agent

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// The pieces of a claim that can be compared with what a tool returned:
// literals (quoted text, paths, hosts, versions, dates), plain numbers, and
// numbers with the word that counts them ("3 tests"). Everything is compared
// in normalised form: lower case, no terminal colours, one space.

// facts is what a claim asserts that a check can look for.
type facts struct {
	literals  []string    // must appear as written, normalised
	paths     []string    // file paths named, also literals
	hosts     []string    // host names and addresses, also literals
	numbers   []string    // standalone numbers, normalised
	counted   []countFact // numbers with the word that counts them
	keyed     []countFact // a word and the number that follows it, as in "port 8080"
	fileLines []fileLine  // path:line cites, checked against the file now
	success   bool        // says a command, test or build succeeded
	testish   string      // "test" or "build" when success names one
	changed   bool        // says the agent changed the paths it names
	negated   bool        // says something did not happen, or is not so
	scope     []string    // packages or folders a test claim names, such as internal/auth
	ran       []string    // commands the claim says were run
}

type countFact struct {
	n    string
	unit string
	// quals are the words that say what about the unit is counted ("added",
	// "passed"); see readCount. scoped: words after it narrow it.
	quals  []string
	scoped bool
}

type fileLine struct {
	path string
	line int
}

func (f facts) any() bool {
	return len(f.literals) > 0 || len(f.numbers) > 0 || len(f.counted) > 0 || len(f.fileLines) > 0 ||
		f.success || (f.changed && len(f.paths) > 0) || len(f.ran) > 0
}

var (
	ansiRe     = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
	spaceRe    = regexp.MustCompile(`\s+`)
	backtickRe = regexp.MustCompile("`([^`\n]{2,200})`")
	dquoteRe   = regexp.MustCompile(`"([^"\n]{2,200})"`)
	urlRe      = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `)\]]+`)
	fileLineRe = regexp.MustCompile(`((?:[\w.\-]+/)*[\w\-]+\.[A-Za-z][A-Za-z0-9]{0,5}):(\d+)(?::\d+)?\b`)
	pathRe     = regexp.MustCompile(`(?:~|\.{1,2})?/?(?:[\w.\-]+/)+[\w.\-]+|\b[\w\-]+\.(?:go|py|js|mjs|ts|tsx|jsx|json|ya?ml|md|toml|txt|sh|rs|java|kt|c|h|cc|cpp|hpp|rb|php|html|css|sql|lock|mod|sum|cfg|ini|conf|xml|env|log|csv|proto|tf|gradle)\b`)
	versionRe  = regexp.MustCompile(`\bv?\d+\.\d+\.\d+(?:[-+][\w.]+)?\b`)
	dateRe     = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2})?)?\b`)
	timeRe     = regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?\b`)
	ipRe       = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	hostRe     = regexp.MustCompile(`\b(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,24}(?::\d+)?\b`)
	numberRe   = regexp.MustCompile(`(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?`)
	keyedRe    = regexp.MustCompile(`\b([a-z][a-z\-]{1,24})\s+((?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?)\b`)
	countRe    = regexp.MustCompile(`((?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?)\s*(%|[a-z][a-z\-]{1,24})`)
	// What a claim says when it reports success; negRe undoes it.
	successRe = regexp.MustCompile(`\b(pass(?:es|ed|ing)?|succeed(?:s|ed)?|successful(?:ly)?|green|exit(?:ed)?(?: with)?(?: code| status)? 0|no (?:errors|failures)|builds? cleanly|compiles? cleanly)\b`)
	negRe     = regexp.MustCompile(`\b(not|never|cannot|can't|couldn't|didn't|doesn't|don't|won't|isn't|aren't|wasn't|fail(?:s|ed|ing|ure|ures)?|error(?:s|ed)?|broken)\b`)
	noFailRe  = regexp.MustCompile(`\bno (?:errors|failures|failed tests?|tests? failed)\b`)
	testRe    = regexp.MustCompile(`\b(tests?|specs?|pytest|jest|unit|suite)\b`)
	buildRe   = regexp.MustCompile(`\b(build|builds|compiles?|compiled|vet|lint)\b`)
	changedRe = regexp.MustCompile(`\b(?:i|i've|i have|we|we've|we have)\s+(?:also\s+|now\s+|just\s+)?(edited|updated|changed|modified|created|wrote|written|added|fixed|deleted|removed|renamed|patched|refactored|rewrote|replaced)\b`)
	// What a claim says when it says a thing did not happen or is not so;
	// such a claim is never verified by output showing the thing.
	negationRe = regexp.MustCompile(`\b(not|never|no|none|nothing|without|didn't|don't|doesn't|isn't|wasn't|aren't|weren't|haven't|hasn't|cannot|can't|untouched|unchanged|unmodified|left alone|neither|nor|avoided|avoid|refrained|as it was|as is|intact|as before|unaltered|preserved|stays the same|remains the same|original)\b`)
	// "I did not change X", "I avoided editing X", "I kept X as it was".
	notChangedRe = regexp.MustCompile(`\b(?:i|we)\s+(?:(?:did\s+not|didn't|have\s+not|haven't|never|did\s+never)\s+(?:\w+\s+)?(?:edit|update|change|modify|create|write|touch|delete|remove|rename|patch|rewrite|replace)` +
		`|(?:avoided|refrained\s+from)\s+(?:\w+ing)` +
		`|(?:kept|left)\s+(?:\S+\s+){1,4}?(?:as\s+it\s+was|as\s+is|unchanged|untouched|intact|alone))`)
	ranRe = regexp.MustCompile(`\b(?:i|i've|i have|we|we've)\s+(?:also\s+|then\s+|just\s+)?(?:ran|run|executed)\b`)
)

// Words that follow a number without counting anything.
var notUnits = map[string]bool{
	"of": true, "and": true, "or": true, "to": true, "in": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "at": true, "on": true, "for": true, "by": true, "out": true, "a": true,
	"an": true, "as": true, "from": true, "with": true, "per": true, "than": true, "more": true,
	"less": true, "times": true, "x": true, "it": true, "that": true, "this": true, "be": true,
}

// keyStop are words a number follows without being their value.
var keyStop = map[string]bool{"has": true, "have": true, "had": true, "all": true, "only": true, "about": true,
	"over": true, "under": true, "after": true, "before": true, "since": true, "took": true, "takes": true,
	"until": true, "last": true, "first": true, "every": true, "each": true, "runs": true, "ran": true, "uses": true,
	"used": true, "at": true, "line": true, "lines": true, "step": true, "version": true}

// fileExts are endings that make a dotted name a file, not a host.
var fileExts = map[string]bool{
	"go": true, "py": true, "js": true, "mjs": true, "ts": true, "tsx": true, "jsx": true, "json": true,
	"yaml": true, "yml": true, "md": true, "toml": true, "txt": true, "sh": true, "rs": true, "java": true,
	"kt": true, "c": true, "h": true, "cc": true, "cpp": true, "hpp": true, "rb": true, "php": true,
	"html": true, "css": true, "sql": true, "lock": true, "mod": true, "sum": true, "cfg": true, "ini": true,
	"conf": true, "xml": true, "env": true, "log": true, "csv": true, "proto": true, "tf": true, "gradle": true,
}

// normText is s as checks compare it.
func normText(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`, "–", "-", "—", "-",
		"\\", "/", " ", " ").Replace(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(strings.ToLower(s), " "))
}

// normNumber drops thousands separators and leading zeros, so 1,204 and
// 1204, or 3.50 and 3.5, compare equal.
func normNumber(n string) string {
	n = strings.ReplaceAll(strings.TrimRight(n, ".,"), ",", "")
	if strings.Contains(n, ".") {
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return n
	}
	if t := strings.TrimLeft(n, "0"); t != "" {
		return t
	}
	return "0"
}

// unitOf is a counting word as compared: lower case and singular.
func unitOf(w string) string {
	w = strings.ToLower(strings.Trim(w, "-"))
	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		return w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "es") && (strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes") || strings.HasSuffix(w, "xes")):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && len(w) > 3:
		return w[:len(w)-1]
	}
	return w
}

// extractFacts reads what a claim asserts that can be checked.
func extractFacts(claim string) facts {
	var f facts
	text := ansiRe.ReplaceAllString(claim, "")
	seen := map[string]bool{}
	addLit := func(s string) {
		s = normText(strings.Trim(s, ",;:!?()[]{}'\""))
		if !strings.HasSuffix(s, "...") {
			s = strings.TrimRight(s, ".")
		}
		if len(s) < 2 || seen[s] {
			return
		}
		seen[s] = true
		f.literals = append(f.literals, s)
	}
	// cut removes what a pattern matched, so its digits are not numbers too;
	// a match each declines stays in the text.
	cut := func(re *regexp.Regexp, each func(m []string) bool) {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			if !each(re.FindStringSubmatch(m)) {
				return m
			}
			return " "
		})
	}
	lower := strings.ToLower(text)
	f.changed = changedRe.MatchString(lower)
	f.negated = negationRe.MatchString(noFailRe.ReplaceAllString(lower, " "))

	if notChangedRe.MatchString(lower) {
		f.changed, f.negated = true, true
	}
	for _, w := range strings.Fields(text) {
		if w = rawPath(strings.Trim(w, "`")); strings.Contains(w, "/") && !strings.Contains(w, "://") {
			f.scope = append(f.scope, w)
		}
	}
	ranCmd := ranRe.MatchString(lower)

	cut(backtickRe, func(m []string) bool {
		addLit(m[1])
		if ranCmd {
			f.ran = append(f.ran, strings.TrimSpace(m[1]))
		}
		// A path or file:line inside backticks is still one.
		if fl := fileLineRe.FindStringSubmatch(m[1]); fl != nil {
			n, _ := strconv.Atoi(fl[2])
			f.fileLines = append(f.fileLines, fileLine{path: fl[1], line: n})
		} else if p := pathRe.FindString(m[1]); p == m[1] {
			f.paths = append(f.paths, rawPath(p))
		}
		return true
	})
	cut(dquoteRe, func(m []string) bool { addLit(m[1]); return true })
	cut(urlRe, func(m []string) bool {
		addLit(m[0])
		if h := hostRe.FindString(strings.ToLower(m[0])); h != "" {
			f.hosts = append(f.hosts, h)
		}
		return true
	})
	cut(fileLineRe, func(m []string) bool {
		n, _ := strconv.Atoi(m[2])
		f.fileLines = append(f.fileLines, fileLine{path: m[1], line: n})
		f.paths = append(f.paths, rawPath(m[1]))
		return true
	})
	cut(ipRe, func(m []string) bool { addLit(m[0]); f.hosts = append(f.hosts, m[0]); return true })
	cut(dateRe, func(m []string) bool { addLit(m[0]); return true })
	cut(versionRe, func(m []string) bool { addLit(m[0]); return true })
	cut(pathRe, func(m []string) bool {
		p := strings.TrimRight(m[0], ".")
		if !looksLikePath(p) {
			return false
		}
		addLit(p)
		f.paths = append(f.paths, rawPath(p))
		return true
	})
	// What is left: host names, then numbers.
	low := strings.ToLower(text)
	for _, h := range hostRe.FindAllString(low, -1) {
		last := h[strings.LastIndex(h, ".")+1:]
		if i := strings.IndexByte(last, ':'); i >= 0 {
			last = last[:i]
		}
		if fileExts[last] || h == "e.g" || h == "i.e" {
			continue
		}
		addLit(h)
		f.hosts = append(f.hosts, h)
	}
	low = hostRe.ReplaceAllString(low, " ")
	low = timeRe.ReplaceAllStringFunc(low, func(m string) string { addLit(m); return " " })
	counted := map[string]bool{}
	for _, at := range countRe.FindAllStringSubmatchIndex(low, -1) {
		unit := low[at[4]:at[5]]
		if notUnits[unit] {
			continue
		}
		cf := readCount(low, at)
		if key := cf.n + " " + cf.unit + " " + strings.Join(cf.quals, " "); !counted[key] {
			counted[key] = true
			f.counted = append(f.counted, cf)
		}
	}
	// "replicas: 3", "failed=0": the label is what is counted.
	for _, at := range evLabelRe.FindAllStringSubmatchIndex(low, -1) {
		w := low[at[2]:at[3]]
		if notUnits[w] {
			continue
		}
		cf := countFact{n: normNumber(low[at[4]:at[5]]), unit: unitOf(w), quals: labelQuals(low, at[0], w)}
		if key := cf.n + " " + cf.unit + " " + strings.Join(cf.quals, " "); !counted[key] {
			counted[key] = true
			f.counted = append(f.counted, cf)
		}
	}
	// "port 8080": a word and the number after it, compared only where the
	// output labels that word with a number.
	for _, m := range keyedRe.FindAllStringSubmatch(low, -1) {
		if notUnits[m[1]] || keyStop[m[1]] {
			continue
		}
		f.keyed = append(f.keyed, countFact{n: normNumber(m[2]), unit: unitOf(m[1])})
	}
	// "replicas: 4" is a key and its value as well.
	for _, m := range evLabelRe.FindAllStringSubmatch(low, -1) {
		if !notUnits[m[1]] && !keyStop[m[1]] {
			f.keyed = append(f.keyed, countFact{n: normNumber(m[2]), unit: unitOf(m[1])})
		}
	}
	nums := map[string]bool{}
	for _, m := range numberRe.FindAllStringIndex(low, -1) {
		// A digit inside a word, as in sha256 or utf8, is not a number said.
		if m[0] > 0 && isWordRune(rune(low[m[0]-1])) {
			continue
		}
		n := normNumber(low[m[0]:m[1]])
		if !nums[n] {
			nums[n] = true
			f.numbers = append(f.numbers, n)
		}
	}
	plain := noFailRe.ReplaceAllString(normText(claim), " ")
	if successRe.MatchString(normText(claim)) && !negRe.MatchString(plain) {
		f.success = true
		switch {
		case testRe.MatchString(plain):
			f.testish = "test"
		case buildRe.MatchString(plain):
			f.testish = "build"
		}
	}
	// A change claim may name a folder (internal/auth), which the file-path
	// reading leaves out.
	if f.changed {
		for _, w := range f.scope {
			if !contains(f.paths, w) {
				f.paths = append(f.paths, w)
			}
		}
	}
	return f
}

// looksLikePath tells a file path from words joined by a slash, as in
// "pass/fail" or "3/5": it is rooted, has a file ending, or has two slashes.
func looksLikePath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") ||
		strings.Count(p, "/") >= 2 {
		return true
	}
	base := p[strings.LastIndex(p, "/")+1:]
	dot := strings.LastIndex(base, ".")
	return dot > 0 && fileExts[strings.ToLower(base[dot+1:])]
}

// rawPath is a path as the claim wrote it, case kept: only quotes and the
// punctuation of the sentence around it are taken off.
func rawPath(p string) string {
	return strings.TrimRight(strings.TrimLeft(strings.TrimSpace(p), "(['\""), ".,;:!?)]}'\"")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || r == '_' }

// evidenceText is one observation's output prepared for the checks.
type evidenceText struct {
	norm    string
	numbers map[string]bool
	counts  map[string][]countAt // unit -> each number counted by it
	labels  map[string][]countAt // word -> each number it labels ("port: 8080")
}

type countAt struct {
	n       string
	excerpt string
	quals   []string
}

// closedQuals are the words a count can be compared by: outcomes of a run,
// actions on a thing and states of one, each form mapped to one word. Only
// counts that share one of these say the same thing; any other word beside
// a count ("found", "in the auth package") leaves it a different count.
var closedQuals = func() map[string]string {
	m := map[string]string{}
	verb := func(base string, double bool) {
		m[base] = base
		switch {
		case strings.HasSuffix(base, "s"), strings.HasSuffix(base, "x"), strings.HasSuffix(base, "ch"), strings.HasSuffix(base, "sh"):
			m[base+"es"] = base
		default:
			m[base+"s"] = base
		}
		stem := base
		switch {
		case double:
			stem = base + base[len(base)-1:]
		case strings.HasSuffix(base, "e"):
			stem = base[:len(base)-1]
		}
		m[stem+"ed"], m[stem+"ing"] = base, base
	}
	for _, v := range []string{"pass", "fail", "deselect", "xfail", "xpass", "succeed", "add", "remove", "delete",
		"change", "modify", "create", "fix", "update", "insert", "rename", "rewrite", "restart", "revert",
		"close", "open", "evict", "terminate", "crash"} {
		verb(v, false)
	}
	verb("skip", true)
	verb("stop", true)
	m["modified"], m["modifies"] = "modify", "modify"
	m["failure"], m["failures"], m["errored"] = "fail", "fail", "error"
	for _, st := range []string{"ready", "down", "up", "healthy", "unhealthy", "available", "unavailable", "pending",
		"running", "broken", "missing", "outdated", "idle", "active", "inactive", "degraded", "unused", "flaky"} {
		m[st] = st
	}
	return m
}()

// qualStop are words beside a count that say nothing about what it counts.
var qualStop = map[string]bool{"has": true, "have": true, "had": true, "all": true, "only": true, "about": true,
	"over": true, "under": true, "after": true, "before": true, "since": true, "now": true, "just": true,
	"also": true, "then": true, "there": true, "here": true, "these": true, "those": true, "its": true,
	"their": true, "our": true, "my": true, "your": true, "some": true, "new": true}

// copulas may stand between a count and its state: "3 replicas are ready".
var copulas = map[string]bool{"are": true, "is": true, "was": true, "were": true, "be": true, "been": true,
	"being": true, "have": true, "has": true, "had": true, "now": true, "still": true, "currently": true,
	"already": true, "all": true}

// scopeWords after a count narrow it: "3 tests in the auth package".
var scopeWords = map[string]bool{"in": true, "for": true, "under": true, "within": true, "across": true,
	"from": true, "of": true, "on": true, "inside": true}

// neutralQual are verbs that only report a count ("runs 3 replicas", "found
// 2 issues"): they leave it a plain count, as "replicas: 3" is.
var neutralQual = map[string]bool{"run": true, "runs": true, "ran": true, "set": true, "sets": true,
	"show": true, "shows": true, "showed": true, "shown": true, "count": true, "counts": true, "counted": true,
	"configured": true, "list": true, "lists": true, "listed": true, "report": true, "reports": true,
	"reported": true, "contain": true, "contains": true, "contained": true, "hold": true, "holds": true,
	"held": true, "found": true, "finds": true, "got": true, "gets": true, "returns": true, "returned": true}

// readCount reads the count a countRe or evCountRe match m found in s: its
// number and unit, the words beside it that say what of the unit is counted
// (the word before the number, the word after the unit past any "are" or
// "is", and the unit itself when it is an outcome, as in "12 passed"), and
// whether words after it narrow it. A state before the unit ("3 ready
// replicas") is taken as its word, and the noun after it as the unit.
func readCount(s string, m []int) countFact {
	c := countFact{n: normNumber(s[m[2]:m[3]]), unit: s[m[4]:m[5]]}
	end := m[1]
	// "12 passed" names an outcome; "14 restarts" or "3 changes" is a plural
	// noun, a plain count.
	if q, ok := closedQuals[c.unit]; ok && !strings.HasSuffix(c.unit, "s") {
		c.quals = append(c.quals, q)
		if w, at := nextWord(s, end); w != "" && !notUnits[w] && closedQuals[w] == "" && plainWord(w) {
			c.unit, end = w, at
		}
	}
	if w := wordBefore(s, m[0]); w != "" {
		c.quals = append(c.quals, w)
	}
	at := end
	for i := 0; i < 4; i++ {
		w, next := nextWord(s, at)
		if w == "" {
			break
		}
		if copulas[w] {
			at = next
			continue
		}
		if q := qualWord(w); q != "" && closedQuals[w] != "" {
			c.quals = append(c.quals, q)
			at = next
			continue
		}
		if scopeWords[w] {
			if w2, _ := nextWord(s, next); w2 != "" && (w2[0] < '0' || w2[0] > '9') {
				c.scoped = true
			}
		} else if q := qualWord(w); q != "" && i == 0 {
			c.quals = append(c.quals, q)
		}
		break
	}
	if c.unit != "%" {
		c.unit = unitOf(c.unit)
	}
	return c
}

// nextWord is the word after s[at], on its line and after a space, with the
// index past it, or -1 when punctuation ends the phrase there; "" when there
// is none.
func nextWord(s string, at int) (string, int) {
	if at < 0 || at >= len(s) || s[at] != ' ' {
		return "", at
	}
	i := at
	for i < len(s) && s[i] == ' ' {
		i++
	}
	j := i
	for j < len(s) && s[j] != ' ' && s[j] != '\n' {
		j++
	}
	w := strings.Trim(s[i:j], ",.;:!?()[]{}'\"`*")
	if w == "" {
		return "", at
	}
	if strings.ContainsAny(s[j-1:j], ",.;:!?)") {
		return w, -1
	}
	return w, j
}

func plainWord(w string) bool {
	if len(w) < 2 {
		return false
	}
	for _, r := range w {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// labelQuals are a label's: the label when it is an outcome or a state, as
// in "failed=3", and the word before it on its line ("tests run: 12").
func labelQuals(s string, start int, label string) []string {
	var q []string
	if c, ok := closedQuals[label]; ok {
		q = append(q, c)
	}
	if w := wordBefore(s, start); w != "" {
		q = append(q, w)
	}
	return q
}

// wordBefore stays on the count's line.
func wordBefore(s string, at int) string {
	head := s[:at]
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[i+1:]
	}
	f := strings.Fields(head)
	if len(f) == 0 || strings.ContainsAny(f[len(f)-1][len(f[len(f)-1])-1:], ".!?;,") {
		return "" // the word ends another phrase
	}
	return qualWord(f[len(f)-1])
}

// qualWord is a word beside a count as compared, or "" for none.
func qualWord(w string) string {
	w = strings.Trim(w, ",.;:!?()[]{}'\"`*")
	if c, ok := closedQuals[w]; ok {
		return c
	}
	if notUnits[w] || qualStop[w] || neutralQual[w] || copulas[w] || scopeWords[w] || !plainWord(w) {
		return ""
	}
	return stemQual(w)
}

// stemQual folds the forms of a word outside closedQuals together, roughly.
func stemQual(w string) string {
	switch {
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "shes"):
		return w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && len(w) > 3:
		return w[:len(w)-1]
	}
	for _, suf := range []string{"ing", "ed"} {
		if strings.HasSuffix(w, suf) && len(w) > len(suf)+2 {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

// closedOf are the words of qs from closedQuals.
func closedOf(qs []string) []string {
	var out []string
	for _, q := range qs {
		if closedQuals[q] == q {
			out = append(out, q)
		}
	}
	return out
}

// sharesQual reports a word two counts share.
func sharesQual(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}

var (
	// "3 passed", "12 tests"; and "tests: 12", "failed=3".
	evCountRe = regexp.MustCompile(`((?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?) *(%|[a-z][a-z\-]{1,24})`)
	evLabelRe = regexp.MustCompile(`([a-z][a-z\-]{1,24}) *[:=] *((?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d+)?)`)
)

func prepareEvidence(content string) *evidenceText {
	e := &evidenceText{norm: normText(content), numbers: map[string]bool{}, counts: map[string][]countAt{}, labels: map[string][]countAt{}}
	// Counts are read line by line, so the words beside one are its own.
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		lines[i] = normText(l)
	}
	lined := strings.Join(lines, "\n")
	for _, m := range numberRe.FindAllStringIndex(e.norm, -1) {
		if m[0] > 0 && isWordRune(rune(e.norm[m[0]-1])) {
			continue
		}
		e.numbers[normNumber(e.norm[m[0]:m[1]])] = true
	}
	excerpt := func(at int) string { return strings.ReplaceAll(excerptAround(lined, at, 60), "\n", " ") }
	add := func(n, unit string, at int, quals []string) {
		if notUnits[unit] {
			return
		}
		u := unitOf(unit)
		if unit == "%" {
			u = "%"
		}
		e.counts[u] = append(e.counts[u], countAt{n: normNumber(n), excerpt: excerpt(at), quals: quals})
	}
	for _, m := range evCountRe.FindAllStringSubmatchIndex(lined, -1) {
		if m[0] > 0 && isWordRune(rune(lined[m[0]-1])) {
			continue
		}
		if notUnits[lined[m[4]:m[5]]] {
			continue
		}
		c := readCount(lined, m)
		e.counts[c.unit] = append(e.counts[c.unit], countAt{n: c.n, excerpt: excerpt(m[0]), quals: c.quals})
	}
	for _, m := range evLabelRe.FindAllStringSubmatchIndex(lined, -1) {
		w, n := lined[m[2]:m[3]], lined[m[4]:m[5]]
		add(n, w, m[0], labelQuals(lined, m[0], w))
		if !notUnits[w] {
			e.labels[unitOf(w)] = append(e.labels[unitOf(w)], countAt{n: normNumber(n), excerpt: excerpt(m[0])})
		}
	}
	return e
}

// hasLiteral reports whether lit appears in the text on word boundaries.
func (e *evidenceText) hasLiteral(lit string) bool {
	for from := 0; ; {
		i := strings.Index(e.norm[from:], lit)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(lit)
		before := i == 0 || !isAlnum(e.norm[i-1]) || !isAlnum(lit[0])
		after := end == len(e.norm) || !isAlnum(e.norm[end]) || !isAlnum(lit[len(lit)-1])
		if before && after {
			return true
		}
		from = i + 1
	}
}

func isAlnum(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z'
}

// excerptAround cuts about width bytes either side of at, on rune boundaries.
func excerptAround(s string, at, width int) string {
	from, to := max(0, at-width), min(len(s), at+width)
	for from > 0 && !isRuneStart(s[from]) {
		from--
	}
	for to < len(s) && !isRuneStart(s[to]) {
		to++
	}
	out := strings.TrimSpace(s[from:to])
	if from > 0 {
		out = "…" + out
	}
	if to < len(s) {
		out += "…"
	}
	return out
}

// splitClaims cuts a reply into the statements it makes, one per sentence,
// list item or table row. Code blocks, questions, headings and courtesies are
// left out: they assert nothing a person would act on.
func splitClaims(text string) []string {
	var out []string
	fence := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = !fence
			continue
		}
		if fence || t == "" || strings.HasPrefix(t, "#") || strings.Trim(t, "|-: ") == "" {
			continue
		}
		t = strings.TrimLeft(t, ">*-+• \t")
		if i := strings.IndexAny(t, ".)"); i > 0 && i <= 3 && allDigits(t[:i]) {
			t = strings.TrimSpace(t[i+1:])
		}
		t = strings.NewReplacer("**", "", "__", "").Replace(t)
		if strings.Contains(t, "|") {
			t = strings.TrimSpace(strings.Join(strings.FieldsFunc(t, func(r rune) bool { return r == '|' }), " · "))
		}
		for _, s := range sentences(t) {
			if keepClaim(s) {
				out = append(out, s)
			}
		}
	}
	const maxClaims = 40
	if len(out) > maxClaims {
		out = out[:maxClaims]
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// sentences splits at ., ! or ? followed by a space and a capital, a digit or
// a backtick, so a path or a version is never cut.
func sentences(t string) []string {
	var out []string
	start := 0
	tick := false
	rs := []rune(t)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '`' {
			tick = !tick
		}
		if tick || (rs[i] != '.' && rs[i] != '!' && rs[i] != '?') || i+2 >= len(rs) || rs[i+1] != ' ' {
			continue
		}
		next := rs[i+2]
		if unicode.IsUpper(next) || unicode.IsDigit(next) || next == '`' {
			out = append(out, strings.TrimSpace(string(rs[start:i+1])))
			start = i + 2
		}
	}
	if rest := strings.TrimSpace(string(rs[start:])); rest != "" {
		out = append(out, rest)
	}
	return out
}

var (
	courtesyRe = regexp.MustCompile(`^(let me know|hope this helps|if you (?:want|need|like|'d like)|would you like|feel free|happy to|here(?:'s| is| are) (?:the|a|what)|in summary|summary:|note:)`)
	// Advice and intentions say what to do or what will be done, not what is.
	adviceRe = regexp.MustCompile(`^(?:please |you (?:can|could|may|might|should|will need to)\b|i(?:'ll| will| can| could| would| suggest| recommend)\b|we(?:'ll| will| can| could| should)\b|consider|try|run|use|check|add|remove|make sure|ensure|install|set|open|restart|create|edit|replace|verify|review|apply|enable|disable|configure|deploy|commit|push|keep|avoid|see|look)\b`)
)

func keepClaim(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	if strings.HasSuffix(low, "?") || strings.HasSuffix(low, ":") || len(strings.Fields(low)) < 3 {
		return false
	}
	return !courtesyRe.MatchString(low) && !adviceRe.MatchString(low)
}
