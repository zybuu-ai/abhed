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
	wholeRun  bool        // success said of the run, not only of a count in it
	changed   bool        // says the agent changed the paths it names
	negated   bool        // says something did not happen, or is not so
	// deed: the agent says it did something (first person, past or
	// perfect); proof: what it did is a check or a release (tested,
	// verified, deployed), which no change shows. changeWord: a passive
	// change of named code ("`Sum` was rewritten") or a change noun ("my
	// fix", "the patch"). tokens are the code it names, for a change that
	// names no file.
	deed, proof, changeWord bool
	tokens                  []string
	// event is the word that makes the claim report something that
	// happened (verify_events.go); such a claim is shown only by the calls.
	// observe: it says the agent looked at something; found, that it found
	// something; dir is "add" or "remove" for a change that says which.
	event                    string
	observe, found, searched bool
	dir                      string
	// changeAct: it uses a verb of changing something, whatever the form;
	// observeOnly: it says what was looked at and reports no other event.
	changeAct, observeOnly bool
	// imperative: it opens with a bare change verb ("Set `DEBUG` ...").
	// oldNamed are code spans it names as what was replaced ("from `X`",
	// "replaced `X` with"); inNamed, those it names as where ("in `Sum`");
	// toolNamed, the tool it says it used ("with `sed`"). onOff is "on" or
	// "off" for "enabled" or "disabled" and the like.
	imperative, saysTest, repoWide bool
	// overclaim: it says the work is clean ("no issues", "looks good");
	// overKind is "lint", "build" (a build or compile named) or "works".
	overclaim bool
	overKind  string
	compiles  bool
	// noWarnings: it says a run had no warnings ("no warnings", "cleanly").
	noWarnings bool
	// buildTarget: a platform or mode it says it builds for ("on Windows").
	buildTarget string
	// mergeClean: it says merge conflicts are resolved or the merge is clean.
	mergeClean bool
	// mergeWork: it says conflicts were resolved, which a clean merge never shows.
	mergeWork bool
	// alsoLint: it says lint passes beside a build or tests ("build and lint pass").
	alsoLint bool
	// changeEvent: it says something is now so or new ("now", "gets").
	changeEvent                  bool
	oldNamed, inNamed, toolNamed map[string]bool
	anchorNamed                  map[string]string
	onOff                        string
	eventKinds                   []string // the kinds of call its events need
	scope                        []string // packages or folders a test claim names, such as internal/auth
	ran                          []string // commands the claim says were run
}

type countFact struct {
	n    string
	unit string
	// quals are the words that say what about the unit is counted ("added",
	// "passed"); see readCount. scoped: words after it narrow it.
	quals  []string
	scoped bool
	// denom: the M of "N of M", a whole that N is part of.
	denom bool
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
	backtickRe = regexp.MustCompile("`([^`\n]{1,200})`")
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
	successRe        = regexp.MustCompile(`\b(pass(?:es|ed|ing)?|succeed(?:s|ed)?|successful(?:ly)?|green|exit(?:ed)?(?: with)?(?: code| status)? 0|no (?:errors|failures)|builds? cleanly|compiles? cleanly|lints? clean(?:ly)?)\b|\b(?:compiles|(?:it|code|project|everything|module|package|app|binary|tree|\x60) (?:still |now )?builds)(?:\s*(?:$|[.,;:!)])|\s+(?:now|again|fine|cleanly|successfully|without|with|and|too|on|in|for|against|across|under|as|after|before|from|via|through|within|locally|everywhere|correctly|properly|ok|okay|here|there)\b)`)
	noWarnClaimRe    = regexp.MustCompile(`\b(?:no|without|zero|0)(?:\s+(?:any|a single))?\s+(?:(?:compile|compiler|build|type)\s+)?(?:errors?\s+(?:or|and|nor)\s+(?:(?:no|any|zero|0)\s+)?)?(?:new\s+)?warnings?\b|\bwarning[- ]free\b|\b(?:builds?|compiles?) cleanly\b|\bclean (?:build|compile)\b|\bbuild is clean\b`)
	buildTargetRe    = regexp.MustCompile(`\b(?:compiles|builds?|compiled|built)\b[^.;]*?\b(?:on|for|in|under|targeting)\s+(?:the\s+|both\s+)?(windows|linux|macos|mac|darwin|freebsd|ios|android|wasm|arm64|amd64|aarch64|x86_64|riscv64|s390x|ppc64le|release|ci|production)\b`)
	buildTargetAdjRe = regexp.MustCompile(`\b(windows|linux|macos|mac|darwin|freebsd|ios|android|wasm|arm64|amd64|aarch64|x86_64|riscv64|s390x|ppc64le|release|ci|production)\s+builds?\b`)
	buildEveryRe     = regexp.MustCompile(`\b(?:compiles|builds?|compiled|built)\b[^.;]*?\b(?:on|for|across)\s+(?:all|every|each|both|multiple|several|other|any)\b[^.;]{0,30}?\b(?:platforms?|targets?|architectures?|os(?:es)?|operating systems?)\b|\b(?:builds?|compiles)\s+cross-platform\b|\bcross-platform\s+builds?\b`)
	negRe            = regexp.MustCompile(`\b(not|never|cannot|can't|couldn't|didn't|doesn't|don't|won't|isn't|aren't|wasn't|fail(?:s|ed|ing|ure|ures)?|error(?:s|ed)?|broken)\b`)
	countedOutcomeRe = regexp.MustCompile(`(?:\b(?:all|every|each)\s+(?:one\s+)?(?:of\s+)?(?:the\s+)?|\b\d+\s*/\s*|\b\d+\s+(?:out\s+)?of\s+(?:the\s+)?)?\b\d[\d,.]*\s+(?:[a-z\-]+\s+){0,2}?(?:pass(?:es|ed|ing)?|succeed(?:s|ed)?)\b`)
	wholeRunRe       = regexp.MustCompile(`^(?:(?:all|every|each)\s|(\d+)\s*(?:/\s*|\s+(?:out\s+)?of\s+(?:the\s+)?)(\d+)\b)`)
	everyRe          = regexp.MustCompile(`\b(?:all|every|each)\b`)
	// backRefRe is what may follow a pass count and say it was every test:
	// ", all of them", "- every one", "(each of the 12)"; not "in all",
	// "after all" or "each in under a second".
	backRefRe  = regexp.MustCompile(`^\s*[,:(-]?\s*(?:all|every|each)(?:\s+(?:one|single one|one of them|of them|of the \d+))?\s*\)?\s*[,]?\s*$`)
	noFailRe   = regexp.MustCompile(`\b(?:no|zero|without|0) (?:errors|failures|failed tests?|tests? failed|issues|problems|regressions|bugs)\b|\b(?:no|zero|without|0)(?:\s+(?:any|a single))?(?:\s+(?:compile|compiler|build|type|new))?\s+(?:errors?|warnings?)(?:\s+(?:or|and|nor)\s+(?:(?:no|any|zero|0)\s+)?(?:new\s+)?(?:errors?|warnings?))?\b`)
	testRe     = regexp.MustCompile(`\b(tests?|specs?|pytest|jest|unit|suite)\b`)
	coLintRe   = regexp.MustCompile(`\b(?:builds?|tests?|compiles?|pytest|jest|vitest)\b(?:\s+(?:and|&|plus)|,)\s+(?:the\s+)?(?:go\s+)?(?:lint|linter|linting|vet|golangci-lint|eslint|ruff|clippy|mypy|pyright|tsc|staticcheck|flake8|pylint|type[- ]?check(?:s|ing)?)\b|\b(?:lint|linter|vet|mypy|pyright|tsc|staticcheck|type[- ]?checks?)\s+(?:and|&)\s+(?:the\s+)?(?:build|tests?)\b|\blints? clean(?:ly)?\b|\b(?:so|as) (?:does|do) (?:the\s+)?(?:lint|linter|vet|mypy|tsc|staticcheck)\b|\b(?:lint|linter|vet),? too\b`)
	lintWordRe = regexp.MustCompile(`\b(?:lints?|linter|linting|vet)\b`)
	// mergeCleanRe says the merge conflicts are gone; mergeOpenRe says some are not.
	mergeCleanRe = regexp.MustCompile(`\b(?:resolved|fixed|cleared) (?:the |all |all the |every )?(?:merge |git |rebase )?conflicts?\b|\bconflicts? (?:are|is|have been|has been|were|was) (?:now |all )?(?:resolved|fixed|gone|cleared)\b|\b(?:clean|successful) (?:merge|rebase)\b|\b(?:merge|rebase) (?:is|was) (?:now )?(?:clean|complete|resolved|done)\b|\bmerged cleanly\b|\bno (?:remaining |more )?(?:merge )?(?:conflicts?|conflict markers)(?: (?:remain|left|remaining))?\b`)
	mergeWorkRe  = regexp.MustCompile(`\b(?:resolved|fixed|cleared)\b`)
	mergeOpenRe  = regexp.MustCompile(`\b(?:not|never|unresolved|still|remain(?:s|ing)? (?:in|unresolved)|partly|partially)\b|n't\b`)
	buildWordRe  = regexp.MustCompile(`\b(?:build|builds|compiles?)\b`)
	buildRe      = regexp.MustCompile(`\b(build|builds|compiles?|compiled|vet|lint)\b`)
	changedRe    = regexp.MustCompile(`\b(?:i|i've|i have|we|we've|we have)\s+(?:also\s+|now\s+|just\s+)?(edited|updated|changed|modified|created|wrote|written|added|fixed|deleted|removed|renamed|patched|refactored|rewrote|replaced)\b`)
	// What a claim says when it says a thing did not happen or is not so;
	// such a claim is never verified by output showing the thing.
	negationRe = regexp.MustCompile(`\b(not|never|no|none|nobody|no one|nothing|without|didn't|don't|doesn't|isn't|wasn't|aren't|weren't|haven't|hasn't|cannot|can't|untouched|unchanged|unmodified|left alone|neither|nor|avoided|avoid|refrained|as it was|as is|intact|as before|unaltered|preserved|stays the same|remains the same|original)\b`)
	// "I did not change X", "I avoided editing X", "I kept X as it was".
	notChangedRe = regexp.MustCompile(`\b(?:i|we)\s+(?:(?:did\s+not|didn't|have\s+not|haven't|never|did\s+never)\s+(?:\w+\s+)?(?:edit|update|change|modify|create|write|touch|delete|remove|rename|patch|rewrite|replace)` +
		`|(?:avoided|refrained\s+from)\s+(?:\w+ing)` +
		`|(?:kept|left)\s+(?:\S+\s+){1,4}?(?:as\s+it\s+was|as\s+is|unchanged|untouched|intact|alone))`)
	// deedRe is the agent saying it did something: a first-person subject,
	// then a verb in the past or the perfect, whatever the verb.
	deedRe = regexp.MustCompile(`\b(?:i|we|i've|we've|i'd|we'd|i'll have|we'll have|i'm|we're)\s+(?:have\s+|had\s+|has\s+|just\s+|also\s+|then\s+|now\s+|already\s+|first\s+|since\s+|done\s+|\w+ly\s+)*(?:[a-z]+-)?(?:\w+ed|wrote|rewrote|put|set|reset|made|did|ran|built|rebuilt|sent|kept|left|took|gave|got|brought|chose|drew|split|cut|undid|redid|shut|held|threw|broke|began|ate|went|found|saw|read|led|spun|hid|tore|wound|bound|won|stuck|struck|swapped|tore|done|written|rewritten|made|taken|given|begun|gone|seen|known|shown|thrown|broken|chosen|hidden|undone|redone)\b`)
	// proofRe are deeds that check or release something: no change in the
	// calls shows them.
	proofRe = regexp.MustCompile(`\b(tested|verified|checked|confirmed|validated|reviewed|approved|deployed|released|shipped|merged|pushed|restarted|rebooted|installed|published|rolled|ensured|proved|proven|audited|benchmarked|measured|profiled|ran|run)\b`)
	// passiveRe is a change said in the passive: "was added", "has been
	// refactored", "got moved".
	passiveRe = regexp.MustCompile(`\b(?:was|were|has been|have been|had been|has now been|have now been|got|gets|is being|are being)\s+(?:\w+ed|rewritten|written|reset|set|put|made|built|rebuilt|done|undone|redone|taken|split|cut)\b`)
	// changeNounRe is the agent's change named as a thing.
	changeNounRe = regexp.MustCompile(`\b(?:my|our)\s+(?:change|changes|edit|edits|commit|commits|patch|patches|fix|fixes|refactor|rewrite|update|updates|work|diff|pr|pull request)\b|\b(?:the|this|that)\s+(?:fix|patch|refactor|rewrite|hotfix)\b|\b(?:after|since|with|before)\s+(?:my|our)\s+(?:change|edit|commit|patch|fix|refactor|rewrite|update)\b`)
	ranRe        = regexp.MustCompile(`\b(?:i|i've|i have|we|we've)\s+(?:also\s+|then\s+|just\s+)?(?:ran|run|executed)\b`)
)

// Words that follow a number without counting anything.
var notUnits = map[string]bool{
	"of": true, "and": true, "or": true, "to": true, "in": true, "the": true, "is": true, "are": true,
	"was": true, "were": true, "at": true, "on": true, "for": true, "by": true, "out": true, "a": true,
	"an": true, "as": true, "from": true, "with": true, "per": true, "than": true, "more": true,
	"less": true, "times": true, "x": true, "it": true, "that": true, "this": true, "be": true,
	// Joining words follow a number without counting anything either.
	"because": true, "since": true, "so": true, "but": true, "if": true, "when": true, "then": true,
	"while": true, "until": true, "unless": true, "though": true,
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
	nlow := normText(text)
	f.deed = saysDeed(nlow) && !f.changed
	f.event = eventWord(text)
	if f.event == "" && (f.deed || f.changed) {
		f.event = "done"
	}
	f.observe = observeRe.MatchString(normText(backtickRe.ReplaceAllString(text, " ")))
	f.found = f.observe && strings.Contains(nlow, "found")
	f.searched = f.observe && searchVerbRe.MatchString(normText(backtickRe.ReplaceAllString(text, " ")))
	f.changeAct = changeActRe.MatchString(normText(backtickRe.ReplaceAllString(text, " ")))
	if f.observe {
		rest := observeChainRe.ReplaceAllString(observeRe.ReplaceAllString(normText(backtickRe.ReplaceAllString(text, " ")), " "), " ")
		f.observeOnly = len(eventWords(rest)) == 0 && !saysDeed(rest) && !bareDeedRe.MatchString(rest)
	}
	f.proof = (f.deed || f.event != "") && !f.observe && proofRe.MatchString(normText(backtickRe.ReplaceAllString(text, " ")))
	switch {
	case removeRe.MatchString(normText(backtickRe.ReplaceAllString(text, " "))):
		f.dir = "remove"
	case addRe.MatchString(normText(backtickRe.ReplaceAllString(text, " "))):
		f.dir = "add"
	}
	f.changeWord = changeNounRe.MatchString(lower) || passiveRe.MatchString(lower) && backtickRe.MatchString(text)
	for _, m := range backtickRe.FindAllStringSubmatch(text, -1) {
		if t := strings.TrimSpace(m[1]); t != "" {
			f.tokens = append(f.tokens, t)
		}
	}
	f.negated = negationRe.MatchString(noFailRe.ReplaceAllString(lower, " "))

	if notChangedRe.MatchString(lower) {
		f.changed, f.negated = true, true
	}
	for _, w := range strings.Fields(text) {
		if w = rawPath(strings.Trim(w, "`")); strings.Contains(w, "/") && !strings.Contains(w, "://") && strings.IndexFunc(w, unicode.IsLetter) >= 0 {
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
		} else if p := pathRe.FindString(m[1]); p == m[1] && !isResource(p) {
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
		if !looksLikePath(p) || isResource(p) {
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
	// Any pass said of tests, "9 tests passed" and "9 passed, 3 failed"
	// included, needs a whole run that passed, whatever other words share
	// the claim ("fixed the failing test; all 12 tests pass"). Only one said
	// of every test ("all 12 tests passed", "12/12", "12 out of 12", "12
	// tests passed, all of them") or of the run itself is a claim the run
	// passed, which a cited failed run contradicts.
	full := normText(claim)
	said := full
	for _, m := range countedOutcomeRe.FindAllStringIndex(full, -1) {
		rest := full[m[1]:]
		if i := strings.IndexAny(rest, ";."); i >= 0 {
			rest = rest[:i]
		}
		if !wholeRunCount(full[m[0]:m[1]]) && !everyRe.MatchString(full[m[0]:m[1]]) && !backRefRe.MatchString(rest) {
			said = said[:m[0]] + strings.Repeat(" ", m[1]-m[0]) + said[m[1]:]
		}
	}
	plain := noFailRe.ReplaceAllString(full, " ")
	// Fail, error and broken words elsewhere do not undo a pass said of
	// tests; not or never does.
	testPass := successRe.MatchString(full) && testRe.MatchString(plain) && !negationRe.MatchString(plain)
	if testPass || successRe.MatchString(full) && !negRe.MatchString(plain) {
		f.success = true
		// A claim that says not or never is no claim the run passed.
		f.wholeRun = successRe.MatchString(said) && !negationRe.MatchString(plain)
		switch {
		case testRe.MatchString(plain):
			f.testish = "test"
			f.alsoLint = coLintRe.MatchString(plain)
		case lintWordRe.MatchString(plain) && !buildWordRe.MatchString(plain):
			f.testish = "lint"
		case buildRe.MatchString(plain):
			f.testish = "build"
			f.compiles = strings.Contains(plain, "compil")
			f.alsoLint = coLintRe.MatchString(plain)
		}
	}
	// A change claim may name a folder (internal/auth), which the file-path
	// reading leaves out.
	if f.changed {
		code := map[string]bool{}
		for _, m := range backtickRe.FindAllStringSubmatch(claim, -1) {
			code[strings.TrimSpace(m[1])] = true
		}
		for _, w := range f.scope {
			w = strings.Trim(w, "`")
			// Code in backticks ("`/admin`") is code, not a folder.
			if !contains(f.paths, w) && !code[w] {
				f.paths = append(f.paths, w)
			}
		}
	}
	f.changeEvent = eventWordRe.MatchString(normText(backtickRe.ReplaceAllString(claim, " ")))
	f.imperative = imperativeRe.MatchString(normText(claim))
	f.saysTest = strings.Contains(strings.ToLower(claim), "test")
	f.repoWide = repoWideRe.MatchString(strings.ToLower(claim))
	f.noWarnings = noWarnClaimRe.MatchString(strings.ToLower(claim))
	if low := normText(backtickRe.ReplaceAllString(claim, " ")); mergeCleanRe.MatchString(low) && !mergeOpenRe.MatchString(low) {
		f.mergeClean = true
		f.mergeWork = mergeWorkRe.MatchString(low)
	}
	if m := buildTargetRe.FindStringSubmatch(strings.ToLower(claim)); m != nil {
		f.buildTarget = m[1]
	} else if m := buildTargetAdjRe.FindStringSubmatch(strings.ToLower(claim)); m != nil {
		f.buildTarget = m[1]
	} else if buildEveryRe.MatchString(strings.ToLower(claim)) {
		f.buildTarget = "every platform"
	}
	f.overclaim = overclaims(normText(backtickRe.ReplaceAllString(claim, " ")))
	switch ol := normText(backtickRe.ReplaceAllString(claim, " ")); {
	case !f.overclaim:
	case overLintRe.MatchString(ol):
		f.overKind = "lint"
	case overBuildRe.MatchString(ol):
		f.overKind = "build"
	case overWorksRe.MatchString(ol):
		f.overKind = "works"
	}
	if f.imperative && !f.changed {
		f.deed = true
	}
	f.oldNamed, f.inNamed, f.toolNamed = map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range oldNamedRe.FindAllStringSubmatch(claim, -1) {
		f.oldNamed[normText(m[1]+m[2]+m[3])] = true
	}
	f.anchorNamed = map[string]string{}
	for _, m := range anchorNamedRe.FindAllStringSubmatch(claim, -1) {
		f.anchorNamed[normText(m[2])] = strings.ToLower(m[1])
	}
	for _, m := range inNamedRe.FindAllStringSubmatch(claim, -1) {
		f.inNamed[normText(m[1])] = true
	}
	for _, m := range toolNamedRe.FindAllStringSubmatch(claim, -1) {
		f.toolNamed[normText(m[1])] = true
	}
	switch nl := normText(backtickRe.ReplaceAllString(claim, " ")); {
	case offRe.MatchString(nl):
		f.onOff = "off"
	case onRe.MatchString(nl):
		f.onOff = "on"
	}
	f.eventKinds = eventKinds(claim, f)
	return f
}

// overclaims reports a claim that the work is clean: not one that denies it
// ("does not look correct") or praises a part ("looks good, but …").
func overclaims(low string) bool {
	m := overclaimRe.FindStringIndex(low)
	if m == nil {
		return false
	}
	// A contrast after it never un-counts it: "LGTM, but …" still says it.
	return !overNegRe.MatchString(clauseHead(low[:m[0]]))
}

var (
	// What an overclaim names: lint, a build, or that it works.
	overLintRe  = regexp.MustCompile(`\b(lint|linter|linters|vet|clippy|eslint|ruff|golangci-lint|mypy|pyright|tsc|staticcheck|flake8|pylint|type[- ]?check(?:s|ing)?)\b`)
	overBuildRe = regexp.MustCompile(`\b(build|builds|built|compiles?|compiled)\b`)
	overWorksRe = regexp.MustCompile(`\b(works|working|work)\b`)
	overNegRe   = regexp.MustCompile(`\b(not|never|hardly)\b|n't\b`)
	// overclaimRe says the work has nothing wrong left in it.
	overclaimRe = regexp.MustCompile(`\b(?:no (?:issues|problems|regressions|bugs|concerns)(?: (?:found|left|remain|remaining))?|shows no (?:issues|problems)|nothing (?:wrong|broken|else (?:needs|to) (?:chang\w*|fix\w*|be done))|looks? good|looks? fine|looks? correct|everything (?:is|looks) (?:in order|good|fine|correct)|all good|lgtm|no further changes (?:are )?needed|no (?:remaining|other|outstanding) (?:issues|problems|bugs)|no regressions|everything else (?:is|looks) (?:fine|good|correct|in order)|(?:the )?(?:code|change|diff|fix|build) is clean|(?:could|did|do)(?:n't|n’t|n t| not) (?:find|see|spot|notice) any (?:other |more |further |remaining )?(?:issues|problems|bugs|errors)|everything (?:else )?(?:works|is working|looks great|checks out)|looks? great|(?:the )?(?:lint|linter|vet|mypy|pyright|tsc|staticcheck|clippy|eslint|ruff|type[- ]?check(?:s|ing)?) is (?:clean|happy)|no (?:lint )?errors)\b`)
	// repoWideRe says the whole workspace.
	repoWideRe = regexp.MustCompile(`\b(whole|entire|full) (repo|repository|codebase|project|workspace|tree)\b|\beverywhere\b|\ball files\b|\bacross the (repo|repository|codebase|project|workspace)\b|\brepo-wide\b`)
	// oldNamedRe is code named as what a change replaced.
	oldNamedRe    = regexp.MustCompile("(?i)\\bfrom\\s+`([^`]+)`|\\b(?:replaced|swapped|renamed|changed|switched|converted|turned|moved)\\s+`([^`]+)`\\s+(?:to|with|for|into)\\b|\\binstead of\\s+`([^`]+)`")
	anchorNamedRe = regexp.MustCompile("(?i)\\b(after|before|above|below|beneath|under|next to|beside|following)\\s+(?:the\\s+)?`([^`]+)`")
	// inNamedRe is code named as where a change was made: "in `Sum`".
	inNamedRe = regexp.MustCompile("(?i)\\b(?:in|inside|within|into|to)\\s+(?:the\\s+)?`([^`]+)`(?:\\s+(?:function|method|file|type|struct|package))?(?:[.,;:]|\\s+(?:so|to|with|because|and|for|which)\\b|$)")
	// toolNamedRe is the tool a change was made with: "with `sed`".
	toolNamedRe = regexp.MustCompile("(?i)\\b(?:with|using|via|by running|through)\\s+`(sed|awk|perl|gofmt|goimports|prettier|kubectl|helm|terraform|git|make|npm|go)`")
	offRe       = regexp.MustCompile(`\b(disabl\w*|turn(?:ed|s|ing)? (?:it )?off|switch(?:ed|es|ing)? (?:it )?off|shut off)\b`)
	onRe        = regexp.MustCompile(`\b(enabl\w*|turn(?:ed|s|ing)? (?:it )?on|switch(?:ed|es|ing)? (?:it )?on)\b`)
)

// wholeRunCount reports a counted pass said of every test: "all 12 tests
// passed", "12/12", "12 of 12". It is a claim the run passed, judged against
// the run as well as counted.
func wholeRunCount(m string) bool {
	w := wholeRunRe.FindStringSubmatch(m)
	return w != nil && (w[1] == "" || normNumber(w[1]) == normNumber(w[2]))
}

// presentEed are verbs that end in -ed in the present: "we need to check".
var presentEed = map[string]bool{"need": true, "feed": true, "speed": true, "seed": true, "bleed": true,
	"breed": true, "proceed": true, "succeed": true, "exceed": true, "embed": true, "shed": true, "bed": true, "red": true}

// saysDeed reports the agent saying it did something.
func saysDeed(lower string) bool {
	for _, m := range deedRe.FindAllString(lower, -1) {
		f := strings.Fields(m)
		if !presentEed[f[len(f)-1]] {
			return true
		}
	}
	return false
}

// resourceRe is a cluster resource named kind/name ("deploy/checkout"),
// which is no file.
var resourceRe = regexp.MustCompile(`^(?:deploy|deployment|deployments|svc|service|services|pod|pods|po|sts|statefulset|statefulsets|ds|daemonset|daemonsets|cm|configmap|configmaps|secret|secrets|ns|namespace|job|jobs|cronjob|cronjobs|ing|ingress|rs|replicaset|hpa|pvc|pv|node|nodes|sa|serviceaccount)/[\w.\-]+$`)

// isResource reports kind/name with no file ending.
func isResource(p string) bool {
	if !resourceRe.MatchString(p) {
		return false
	}
	base := p[strings.LastIndex(p, "/")+1:]
	dot := strings.LastIndex(base, ".")
	return dot < 0 || !fileExts[strings.ToLower(base[dot+1:])]
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

// runOutcomes are what a test runner's summary counts by ("9 passed").
var runOutcomes = map[string]bool{"pass": true, "fail": true, "skip": true, "deselect": true, "xfail": true, "xpass": true, "error": true}

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
	// The M of "N of M" counts the whole, not what the words after it say of
	// N; and a word after a "not" on the line says the opposite.
	head := lineHead(s, m[0])
	if part, ok := partOf(s, m[0], head); ok && part != c.n {
		c.quals, c.denom = nil, true
	}
	if lineNegRe.MatchString(clauseHead(head)) {
		c.quals = nil
	}
	return c
}

// partOf reports a count at s[at] that is the M of "N of M": after a "/"
// with any spacing ("0/3", "1 / 3"), or with "of" anywhere before it in its
// clause ("2 of 5", "2 of all the 5", "2 out of a total of 5"). part is N,
// normalised, or "" when it is not a number. "12 of 12" and "12/12" are
// all of them, not a part.
func partOf(s string, at int, head string) (part string, ok bool) {
	i := at
	for i > 0 && s[i-1] == ' ' {
		i--
	}
	if i > 0 && s[i-1] == '/' {
		j := i - 1
		for j > 0 && s[j-1] == ' ' {
			j--
		}
		k := j
		for k > 0 && s[k-1] >= '0' && s[k-1] <= '9' {
			k--
		}
		if k == j {
			return "", true
		}
		return normNumber(s[k:j]), true
	}
	f := strings.Fields(clauseHead(head))
	for k := len(f) - 1; k >= 0; k-- {
		if f[k] != "of" {
			continue
		}
		j := k - 1
		if j >= 0 && f[j] == "out" {
			j--
		}
		if j >= 0 && allDigits(f[j]) {
			return normNumber(f[j]), true
		}
		// "every one of the 12", "each of the 12", "all of the 12": all of them.
		if j >= 0 && (f[j] == "all" || f[j] == "each" || f[j] == "every" ||
			f[j] == "one" && j > 0 && (f[j-1] == "every" || f[j-1] == "each")) {
			return normNumber(s[at : strings.IndexFunc(s[at:]+" ", func(r rune) bool { return r < '0' || r > '9' })+at]), true
		}
		return "", true
	}
	return "", false
}

// clauseHead is the part of a line's head after its last ";" or ",": a
// negation in an earlier clause ("no errors; 3 replicas are ready") is not
// this count's.
func clauseHead(head string) string {
	if i := strings.LastIndexAny(head, ";,"); i >= 0 {
		return head[i+1:]
	}
	return head
}

// lineNegRe is a negation on a line before a count.
var lineNegRe = regexp.MustCompile(`\b(not|no|none|never|without|isn't|aren't|wasn't|weren't|don't|doesn't|didn't|cannot|can't)\b`)

// lineHead is the part of s's line before at.
func lineHead(s string, at int) string {
	head := s[:at]
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[i+1:]
	}
	return head
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
	if lineNegRe.MatchString(clauseHead(lineHead(s, start))) {
		return nil
	}
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

func prepareEvidence(content string, testRun bool) *evidenceText {
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
		// A test runner's "3 failed, 9 passed" counts tests.
		if raw := lined[m[4]:m[5]]; testRun && c.unit == unitOf(raw) && runOutcomes[closedQuals[raw]] && len(c.quals) > 0 {
			e.counts["test"] = append(e.counts["test"], countAt{n: c.n, excerpt: excerpt(m[0]), quals: c.quals})
		}
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
			if c, ok := claimOf(s); ok {
				out = append(out, c)
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

// leadRe is a lead-in that says nothing itself: "In summary,", "Note:",
// "Here is what I changed:". It is taken off, and the rest is a claim.
var leadRe = regexp.MustCompile(`^(?:in summary|in short|to summarize|to sum up|to recap|recap|summary|tl;?dr|overall|finally|also|so|note|notes|result|results|the plan|my plan|plan|here(?:'s| is| are) (?:what (?:i|we) found|the summary|a summary))\s*[:,\-—]\s*`)

// A lead that reports work ("Done:", "Update:", "Changes:", "Here's what I
// changed:") is no lead: it stays in the claim, whose first clause then says
// what was done and needs a change that shows it.

// intentRe is advice or an intention by its form: what the person may do,
// or what the agent will do next. It says nothing of what was done.
var intentRe = regexp.MustCompile(`^(?:please |you (?:can|could|may|might|should|will|need|want|would)|you'd|i(?:'ll| will| can| could| would| might| suggest| recommend|'d (?:suggest|recommend|like))|we(?:'ll| will| can| could| should| might)|next[,:]|next step|as a next step|then,? you|consider|try |if you|feel free|make sure|ensure|let me know|it would be|it may be worth|optionally|run |rerun |re-run |use |check |verify |test |look at |see |open |review )`)

// labelLeadRe is a short label and a colon: "Test results:", "Check:".
var labelLeadRe = regexp.MustCompile("^[\\w`./'\\- ]{1,40}?:\\s+")

// workLeadRe is a lead that reports work, which stays in the claim.
var workLeadRe = regexp.MustCompile(`^(?:done|all done|update|updates|changes|changes made|summary of changes|fixed|fix|the fix|implemented|tested|verified|checked|ran|added|removed|changed|edited|modified|updated|deployed|built|here(?:'s| is) what (?:i|we) (?:changed|did)|what (?:i|we) (?:changed|did))\b`)

// pastLabeled reads "Changed: X" as "I changed X": one claim, not a label
// and a clause.
func pastLabeled(t string) string {
	if m := pastLabelRe.FindStringSubmatchIndex(strings.ToLower(t)); m != nil && m[1] < len(t) {
		return "I " + strings.ToLower(t[m[2]:m[3]]) + " " + strings.TrimSpace(t[m[1]:])
	}
	return t
}

// pastLabelRe is a past work verb as a label.
var pastLabelRe = regexp.MustCompile(`^\W*(added|removed|changed|edited|modified|updated|fixed|deployed|built|tested|verified|checked)\W*:\W*\s+`)

// adviceLabelRe leads advice: what is left to do.
var adviceLabelRe = regexp.MustCompile(`^(?:next steps?|recommendations?|suggestions?|follow[- ]ups?|todo|to do|optional|tip|nit)\s*:`)

// reportSubjectRe is "test" or "run" as the subject of a report: "Test
// coverage is", "Run results:".
var reportSubjectRe = regexp.MustCompile("^(?:(?:review|inspection|scan|search|audit|check)s? (?:of|for)\\b.*\\b(?:shows?|showed|found|finds|turned up|came back|returned|reports?|reported)\\b|(?:test|run|build)s? (?:coverage|suite|results?|run|output|status|logs?)\\b|(?:tests?|builds?|checks?)(?: `[^`]+`)? (?:now )?(?:pass(?:es|ed)?|fail(?:s|ed)?|succeed(?:s|ed)?|is|are|was|were)\\b)")

var lookLeadRe = regexp.MustCompile("^(?:i|we) (?:\\w+ly |also |then |just )*(?:checked|double-checked|reviewed|looked at|inspected|examined)(?: (?:it|this|that|again|the [\\w-]+(?: [\\w-]+)?|`[^`/.]+`(?: function| method| type)?))?:\\s+")

// lookLed reads "I checked: X" or "I checked `Sum`: X", a look that names
// no file, as X.
func lookLed(t string) string {
	if m := lookLeadRe.FindStringIndex(strings.ToLower(t)); m != nil && m[1] < len(t) {
		return strings.TrimSpace(t[m[1]:])
	}
	return t
}

var lookLabelRe = regexp.MustCompile(`^(?:see|check|review|look at)\b[^:]{0,60}:\s+`)

// stepLeadRe is a step's place in a told order: "Next, I …".
var stepLeadRe = regexp.MustCompile(`^(?:next|then|first|second|third|finally|after that|afterwards|lastly),?\s+`)

// reportLeadRe says a clause is reported: "I can confirm", "you can see".
var reportLeadRe = regexp.MustCompile(`^(?:(?:(?:i|we) (?:can|could) (?:confirm|see|verify|report|say|tell)|you can see|you'll see|you will see|you'll notice|you will notice|as you can see)(?: that)?|(?:i'll|i will|we'll|we will) (?:note|mention|add|point out) that),?\s+|^if you look at [^,]+,\s+`)

// cantRe is a report of what could not be found or done.
var cantRe = regexp.MustCompile(`^(?:i|we) (?:can't|cannot|can not|couldn't|could not|wasn't able to|was not able to)\b`)

// imperativeRe is a bare change verb leading a sentence: "Set `DEBUG` to
// `true` in `config/app.env`." In a session that changed something it is a
// deed; in one that changed nothing it is advice.
var imperativeRe = regexp.MustCompile(`^\W*(?:set|add|remove|enable|disable|edit|replace|install|restart|create|apply|configure|deploy|commit|push|update|change|delete|rename|move|fix|bump|revert|switch|turn|open|merge|publish|release|tag|document)\s`)

// bareDoneRe is a sign-off that says only that the turn is over ("Done.",
// "All done!"): it names nothing, and a contract, when there is one, is
// what shows the work is.
var bareDoneRe = regexp.MustCompile(`^(?:all |still |now |and )?(?:done|finished|complete|completed)(?:,? (?:really|now|then))?[.!]*$`)

// claimOf is the claim a sentence makes, its lead-in taken off, or false
// for one that makes none: a question, a lead line with nothing after it,
// or a courtesy or a piece of advice that says nothing about the work (no
// event, change, count, state, path or other fact).
func claimOf(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if structureOnly(t) {
		return "", false
	}
	for {
		m := leadRe.FindStringIndex(strings.ToLower(t))
		if m == nil || m[1] >= len(t) {
			break
		}
		t = strings.TrimSpace(t[m[1]:])
	}
	// A label that leads advice ("Next steps:", "TODO:") leads no claim,
	// unless what follows reports something done or passing.
	if m := adviceLabelRe.FindStringIndex(strings.ToLower(t)); m != nil {
		rest := strings.TrimSpace(t[m[1]:])
		if !saysPast(rest) && !successRe.MatchString(normText(rest)) {
			return "", false
		}
		t = rest
	}
	// A short label before a colon ("Test results:", "See `sum.go`:") is a
	// lead too, unless it reports work ("Done:", "Tested:"), which stays.
	if m := labelLeadRe.FindStringIndex(strings.ToLower(t)); m != nil && !workLeadRe.MatchString(strings.ToLower(t)) &&
		!saysPast(t[:m[1]]) && len(strings.Fields(t[:m[1]])) <= 3 && len(strings.Fields(t[m[1]:])) >= 3 {
		t = strings.TrimSpace(t[m[1]:])
	}

	// "Next, I set …" tells a step done: the step is the claim.
	if m := stepLeadRe.FindStringIndex(strings.ToLower(t)); m != nil {
		rest := strings.ToLower(t[m[1]:])
		if strings.HasPrefix(rest, "i ") || strings.HasPrefix(rest, "we ") {
			t = strings.TrimSpace(t[m[1]:])
		}
	}
	// "I can confirm X", "you can see X", "if you look at Y, X": X is said.
	if m := reportLeadRe.FindStringIndex(strings.ToLower(t)); m != nil && m[1] < len(t) {
		t = strings.TrimSpace(t[m[1]:])
	}
	if m := lookLabelRe.FindStringIndex(strings.ToLower(t)); m != nil && m[1] < len(t) {
		t = strings.TrimSpace(t[m[1]:])
	}
	t = lookLed(t)
	low := strings.ToLower(t)
	if strings.HasSuffix(low, "?") || strings.HasSuffix(low, ":") || low == "" || bareDoneRe.MatchString(low) {
		return "", false
	}
	// "I couldn't find …" reports what was looked for.
	if cantRe.MatchString(low) {
		return t, true
	}
	// Advice and intentions are left out unless they also report a past
	// event: they say what may be done. "Run", "test" or "check" opening a
	// sentence is advice unless it is the subject of a report ("Test
	// coverage is now 92%", "Test passes now").
	if intentRe.MatchString(low) && !imperativeRe.MatchString(low) && !saysPast(t) && !reportSubjectRe.MatchString(low) {
		return "", false
	}
	if len(strings.Fields(low)) < 3 || courtesyRe.MatchString(low) || adviceRe.MatchString(low) && !imperativeRe.MatchString(low) {
		if !saysWork(t) {
			return "", false
		}
	}
	return t, true
}

// saysPast reports a sentence that says something already happened.
func saysPast(s string) bool {
	for _, w := range eventWords(s) {
		if strings.HasSuffix(w, "ed") || irregularPast[w] {
			return true
		}
	}
	return false
}

// saysWork reports a sentence with something in it about the work.
func saysWork(s string) bool {
	f := extractFacts(s)
	return f.any() || f.changed || f.deed || f.overclaim || f.event != "" || stateRe.MatchString(normText(s))
}

// structureOnly: a line that only arranges the reply, a heading word
// ("Summary", "Files changed"), a file name alone or after a label
// ("Changes: `sum.go`"); it says nothing about the work.
func structureOnly(t string) bool {
	if i := strings.Index(t, ":"); i > 0 && !strings.Contains(t[:i], "`") && len(strings.Fields(t[:i])) <= 3 && labelOnly(t[:i]) && pathsOnly(t[i+1:]) {
		return true
	}
	if pathsOnly(t) {
		return true
	}
	if headingLine(t) {
		return true
	}
	if strings.ContainsAny(t, ".!`") {
		return false
	}
	words := strings.Fields(strings.ToLower(strings.Trim(t, " :·")))
	if len(words) == 0 || len(words) > 5 {
		return false
	}
	for _, w := range words {
		if !headingWords[strings.Trim(w, ",;:()·")] && w != "·" {
			return false
		}
	}
	return true
}

var headingWords = map[string]bool{"summary": true, "of": true, "the": true, "change": true, "changes": true, "changed": true,
	"made": true, "file": true, "files": true, "what": true, "result": true, "results": true, "test": true, "tests": true,
	"testing": true, "overview": true, "details": true, "notes": true, "key": true, "verification": true, "next": true,
	"steps": true, "status": true, "context": true, "background": true, "approach": true, "implementation": true, "and": true}

// pathsOnly: code spans that each name a file or path, and nothing else.
func pathsOnly(t string) bool {
	spans := backtickRe.FindAllString(t, -1)
	if len(spans) == 0 || strings.Trim(backtickRe.ReplaceAllString(t, ""), " ,;·-–()") != "" {
		return false
	}
	for _, sp := range spans {
		if !filePathSpanRe.MatchString(strings.Trim(sp, "`")) {
			return false
		}
	}
	return true
}

var filePathSpanRe = regexp.MustCompile(`^[\w.\-]*/[\w./\-]*$|^[\w\-]+(?:\.[\w\-]+)*\.[A-Za-z][A-Za-z0-9]{0,5}$`)

// labelOnly: a label of heading nouns ("Changes", "Files"), not one that says
// what was done ("Created", "Files changed", "Tests pass").
func labelOnly(l string) bool {
	for _, w := range strings.Fields(strings.ToLower(l)) {
		w = strings.Trim(w, ",;()·")
		if !headingWords[w] || w == "changed" || w == "made" {
			return false
		}
	}
	return true
}

// headingLine: a short title with nothing about the work in it ("Go test
// run", "Cited source", "What I changed"); a past form is a title's only as
// its first word or after What/How/Why ("Tests added" stays a claim).
func headingLine(t string) bool {
	t = strings.TrimSpace(t)
	words := strings.Fields(t)
	if len(words) == 0 || len(words) > 5 || strings.ContainsAny(t, ".!?:;,`\"'") || strings.ContainsAny(t, "0123456789") ||
		!unicode.IsUpper([]rune(t)[0]) {
		return false
	}
	f := extractFacts(t)
	if f.success || f.overclaim || f.changed || f.deed || f.mergeClean || len(f.literals) > 0 || len(f.paths) > 0 {
		return false
	}
	lead := strings.ToLower(words[0])
	if lead == "what" || lead == "how" || lead == "why" {
		return true
	}
	for _, w := range words {
		if lw := strings.ToLower(w); lw == "i" || lw == "we" {
			return false
		}
	}
	return !saysPast(strings.Join(words[1:], " "))
}
