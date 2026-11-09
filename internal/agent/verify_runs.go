package agent

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Test and build runs, as the checks read them: what kind of run a command
// is, and which packages or folders it covers. A run that filters its tests,
// lists them, compiles without running them or is given anything not read
// here covers nothing a claim can rest on.

// commandKind says whether one simple command runs tests ("test"), builds
// ("build"), or checks without building ("lint"), by its program and
// subcommand; "" for none of them. A claim of one kind is never met by a run
// of another.
func commandKind(words []string) string {
	if len(words) == 0 {
		return ""
	}
	prog := filepath.Base(words[0])
	sub, third := "", ""
	if len(words) > 1 {
		sub = words[1]
	}
	if len(words) > 2 {
		third = words[2]
	}
	switch prog {
	case "go":
		switch sub {
		case "test":
			return "test"
		case "build", "install":
			return "build"
		case "vet":
			return "lint"
		}
	case "cargo":
		switch sub {
		case "test", "nextest":
			return "test"
		case "build":
			return "build"
		case "check", "clippy":
			return "lint"
		}
	case "npm", "pnpm", "yarn", "bun":
		script := sub
		if sub == "run" {
			script = third
		}
		switch {
		case script == "test" || script == "t" || strings.HasPrefix(script, "test:"):
			return "test"
		case script == "build" || strings.HasPrefix(script, "build:"):
			return "build"
		case script == "lint" || script == "typecheck":
			return "lint"
		}
	case "make", "gmake":
		switch sub {
		case "test", "check", "tests":
			return "test"
		case "", "build", "all":
			return "build"
		case "lint", "vet":
			return "lint"
		}
	case "pytest", "jest", "vitest", "mocha", "rspec", "phpunit", "ctest":
		return "test"
	case "python", "python3":
		if sub == "-m" && (third == "pytest" || third == "unittest") {
			return "test"
		}
	case "tsc":
		// tsc --noEmit type-checks without building.
		for _, w := range words[1:] {
			if w == "--noEmit" {
				return "lint"
			}
		}
		return "build"
	case "javac", "gcc", "clang":
		return "build"
	case "eslint", "ruff", "mypy", "flake8", "pylint", "golangci-lint", "shellcheck", "rubocop", "staticcheck":
		return "lint"
	case "mvn", "gradle", "./gradlew", "gradlew", "dotnet":
		switch sub {
		case "test":
			return "test"
		case "build", "package", "compile", "assemble":
			return "build"
		}
	}
	return ""
}

// colourEnv are variables that change only how a run's output is coloured,
// or point Python's imports at the folder itself (PYTHONPATH=.), never what
// runs or how it is judged. Any other variable set for a run
// (GOFLAGS=-run=X, PYTEST_ADDOPTS='-k x', CI, UPDATE) may filter it or change
// what passes, so the run is not taken as a whole one.
var colourEnv = map[string]bool{"NO_COLOR": true, "FORCE_COLOR": true, "CLICOLOR": true, "CLICOLOR_FORCE": true, "PYTHONPATH": true}

// readerEnv leaves a reader a reader: colour, and a PYTHONPATH inside the
// folder (PYTHONPATH=src), which may change what is imported but not where.
func readerEnv(names []string) bool {
	for _, n := range names {
		if !colourEnv[n] && n != "PYTHONPATH~" {
			return false
		}
	}
	return true
}

func harmlessEnv(names []string) bool {
	for _, n := range names {
		if !colourEnv[n] {
			return false
		}
	}
	return true
}

// scopePat is a folder a run covers, and whether it covers what is beneath.
type scopePat struct {
	dir string
	rec bool
}

// goRunFlags are the go test and go build flags that leave what runs as it
// is; with a value, true. -run, -skip, -short, -list, -c, -n, -exec, -o,
// -args and the rest are filters, listings, dry runs or writes.
var goRunFlags = map[string]bool{
	"count": true, "race": false, "timeout": true, "cover": false, "covermode": true, "coverpkg": true,
	"failfast": false, "benchmem": false, "benchtime": true, "cpu": true, "p": true, "parallel": true,
	"tags": true, "json": false, "shuffle": true, "vet": true, "mod": true, "trimpath": false, "v": false,
}

// runScope reads a test or build run's command, run in dir: the folders it
// covers, or ok false for one this reading does not take as a whole run.
func runScope(words []string, dir string) (pats []scopePat, ok bool) {
	if len(words) == 0 || dir == "" {
		return nil, false
	}
	whole := []scopePat{{dir: dir, rec: true}}
	prog := filepath.Base(words[0])
	switch prog {
	case "go":
		var ops []string
		for i := 2; i < len(words); i++ {
			a := words[i]
			if !strings.HasPrefix(a, "-") {
				ops = append(ops, a)
				continue
			}
			name, _, eq := strings.Cut(strings.TrimLeft(a, "-"), "=")
			valued, known := goRunFlags[name]
			if !known {
				return nil, false
			}
			if valued && !eq {
				i++
			}
		}
		if len(ops) == 0 {
			return []scopePat{{dir: dir}}, true // the package in the folder
		}
		for _, op := range ops {
			if !strings.HasPrefix(op, ".") {
				return nil, false // an import path, not read as a folder
			}
			rec := op == "./..." || op == "..." || strings.HasSuffix(op, "/...")
			op = strings.TrimSuffix(strings.TrimSuffix(op, "..."), "/")
			pats = append(pats, scopePat{dir: filepath.Join(dir, op), rec: rec})
		}
		return pats, true
	case "python", "python3":
		if len(words) > 2 && words[1] == "-m" && words[2] == "unittest" {
			// A pattern selects; a start folder other than the workspace
			// narrows it; test names cover only themselves.
			u, ok := parseUnittest(words)
			if !ok || u.pattern != "" {
				return nil, false
			}
			if len(u.names) > 0 {
				for _, n := range u.names {
					if !insidePath(n) || !testName(n) {
						return nil, false
					}
					pats = append(pats, scopePat{dir: filepath.Join(dir, unittestPath(n))})
				}
				return pats, true
			}
			if u.start != "" && filepath.Clean(u.start) != "." {
				return []scopePat{{dir: filepath.Join(dir, u.start), rec: true}}, true
			}
			return whole, true
		}
		fallthrough
	case "pytest":
		start := 1
		if prog != "pytest" {
			start = 3 // python -m pytest
		}
		for _, a := range words[start:] {
			switch {
			case a == "-q" || a == "-v" || a == "-vv" || a == "-x" || a == "--quiet" || a == "--verbose":
			case strings.HasPrefix(a, "-"):
				return nil, false // -k, -m, --collect-only and the rest select or list
			default:
				pats = append(pats, scopePat{dir: filepath.Join(dir, strings.TrimSuffix(a, "/")), rec: true})
			}
		}
		if len(pats) == 0 {
			return whole, true
		}
		return pats, true
	case "cargo":
		for _, a := range words[2:] {
			switch a {
			case "--release", "-q", "--quiet", "-v", "--verbose", "--locked", "--offline", "--frozen", "--all-features", "--workspace":
			default:
				return nil, false // --no-run, --lib, --test, -p and filters
			}
		}
		return whole, true
	case "jest", "vitest":
		for _, a := range words[1:] {
			if a != "--ci" && a != "--silent" && a != "--runInBand" && a != "run" {
				return nil, false // a pattern, a name filter or a listing
			}
		}
		return whole, true
	case "npm", "pnpm", "yarn", "bun":
		n := 2
		if len(words) > 1 && words[1] == "run" {
			n = 3
		}
		if len(words) != n {
			return nil, false // what follows reaches the script as filters
		}
		return whole, true
	case "tsc":
		for _, a := range words[1:] {
			if a != "--noEmit" && a != "-p" && a != "." && a != "--pretty" {
				return nil, false
			}
		}
		return whole, true
	case "golangci-lint", "ruff", "eslint", "flake8", "pylint", "mypy", "shellcheck", "rubocop", "staticcheck":
		start := 1
		if prog == "golangci-lint" || prog == "ruff" {
			start = 2 // golangci-lint run, ruff check
			if len(words) < 2 || words[1] != "run" && words[1] != "check" {
				return nil, false
			}
		}
		for _, a := range words[start:] {
			switch {
			case strings.HasPrefix(a, "-"):
				return nil, false
			case a == "." || a == "./..." || a == "...":
				pats = append(pats, scopePat{dir: dir, rec: true})
			default:
				pats = append(pats, scopePat{dir: filepath.Join(dir, strings.TrimSuffix(strings.TrimSuffix(a, "..."), "/")), rec: true})
			}
		}
		if len(pats) == 0 {
			return whole, true
		}
		return pats, true
	}
	// make, tsc, mvn, gradle, dotnet and the rest: the bare run only.
	if len(words) <= 2 {
		return whole, true
	}
	return nil, false
}

// unittestPath is the file a unittest name stands for: a dotted module
// (tests.test_text) as its file (tests/test_text.py), a path as it is. A
// name of a class or a test in a module is a path of its own
// (tests/test_text/TextTest.py), so it covers only a claim that names it.
func unittestPath(n string) string {
	n = strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(n), "./"), "/")
	if strings.Contains(n, "/") || strings.HasSuffix(n, ".py") {
		return n
	}
	return strings.ReplaceAll(n, ".", "/") + ".py"
}

// claimScope is where a claim's named packages or folders are, in the
// workspace; none names the whole of it. One named with "/..." (pkg/x/...)
// keeps it: it is that folder and everything beneath.
func claimScope(scope []string, workDir string) []string {
	var out []string
	for _, s := range scope {
		s = slashes(s)
		rec := strings.HasSuffix(s, "...")
		s = strings.TrimSuffix(strings.TrimSuffix(s, "..."), "/")
		if s == "" || s == "." {
			continue
		}
		if !filepath.IsAbs(s) {
			s = filepath.Join(workDir, strings.TrimPrefix(s, "./"))
		}
		s = filepath.Clean(s)
		if rec {
			s += "/..."
		}
		out = append(out, s)
	}
	return out
}

// recursiveScope splits a claim's scope entry into its folder and whether
// it names everything beneath.
func recursiveScope(c string) (string, bool) {
	if d, ok := strings.CutSuffix(c, "/..."); ok {
		return d, true
	}
	return c, false
}

// coversScope reports a run that covers every folder a claim names, by path,
// or the whole workspace when it names none.
func coversScope(pats []scopePat, scope []string, workDir string) bool {
	// A claim of everything beneath a folder needs a run of everything
	// beneath it, or beneath a folder above it.
	covered := func(c string) bool {
		c, rec := recursiveScope(c)
		for _, p := range pats {
			if rec && !p.rec {
				continue
			}
			if c == p.dir || p.rec && strings.HasPrefix(c, p.dir+"/") {
				return true
			}
		}
		return false
	}
	if len(scope) == 0 {
		for _, p := range pats {
			if p.rec && p.dir == filepath.Clean(workDir) {
				return true
			}
		}
		return false
	}
	for _, c := range scope {
		if !covered(c) {
			return false
		}
	}
	return true
}

// sameScope reports a run that covers exactly what a claim names: the whole
// workspace for a claim that names nothing, else the same folders.
func sameScope(pats []scopePat, scope []string, workDir string) bool {
	if len(scope) == 0 {
		return len(pats) == 1 && pats[0].rec && pats[0].dir == filepath.Clean(workDir)
	}
	if len(pats) != len(scope) {
		return false
	}
	for i, p := range pats {
		c, rec := recursiveScope(scope[i])
		if p.dir != c || rec && !p.rec {
			return false
		}
	}
	return true
}

// testScope is what a test claim is judged against: the folders, packages,
// modules and files it names; or, for "the tests" with none named, the test
// runs the person asked for; or the whole workspace. A claim that names a
// test as one among more ("including tests.test_new") names the whole.
func (ix *recordIndex) testScope(f facts) []string {
	if f.inclusive {
		return nil
	}
	scope := append(slices.Clone(f.scope), f.modules...)
	if len(scope) == 0 && f.theTests && f.testish == "test" {
		return promptTests(ix.prompts)
	}
	return scope
}

var (
	// The test runs a person's message asks for, by command: go test's,
	// unittest's and pytest's arguments, and "run tests/test_x.py".
	goTestArgsRe   = regexp.MustCompile("\\bgo\\s+test((?:\\s+[^\\s;&|`'\"]+)*)")
	unittestArgsRe = regexp.MustCompile("\\bpython3?\\s+-m\\s+unittest((?:\\s+[^\\s;&|`'\"]+)*)")
	pytestArgsRe   = regexp.MustCompile("\\bpytest((?:\\s+[^\\s;&|`'\"]+)*)")
	runTargetRe    = regexp.MustCompile("(?i)\\brun\\s+([^\\s;&|`'\"]+)")
	// promptWholeRe asks for more than the runs named: every test, the
	// suite, the rest, or that nothing else break.
	promptWholeRe = regexp.MustCompile(`\b(?:all|every|each|whole|entire|full|complete|suites?|everything|everywhere|anything|nothing|both|other|others|remaining|rest|keep|keeps|stay|stays|green)\b|\bbreak\w*|\bregress\w*`)
)

// promptTests are the test runs the person asked for, across every message
// they typed: test modules and files given to unittest, pytest or "run",
// and go test's folders. None, so "the tests" means them all, when any
// message names no test run, asks for more than it names (every test, the
// suite, everything else), or names a filter or a whole-suite run. A later
// message never narrows what an earlier one asked: the runs are unioned,
// and one that names none widens to the whole.
func promptTests(prompts []string) []string {
	var out []string
	for _, p := range prompts {
		names, ok := messageTests(p)
		if !ok {
			return nil
		}
		for _, n := range names {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// messageTests reads one message's test runs; ok false when it asks for the
// whole, or names none. It narrows only when every mention of tests in the
// message is a runner command read here: whatever is left once those
// commands are taken out may say "the tests" or "run the tests" and nothing
// else about tests. Another runner or target (npm test, make test, tox),
// CI, a test file outside a command, "the api tests", "integration" or
// "e2e", or "also" and "too" leave the whole suite.
func messageTests(p string) (names []string, ok bool) {
	if promptWholeRe.MatchString(strings.ToLower(p)) {
		return nil, false
	}
	arg := func(a string) string {
		a = strings.TrimRight(a, ",:;)!?")
		if !strings.HasSuffix(a, "...") {
			a = strings.TrimRight(a, ".")
		}
		return a
	}
	rest := []byte(p)
	blank := func(from, to int) {
		for k := from; k < to; k++ {
			rest[k] = ' '
		}
	}
	// args walks a command's arguments from the end of its program words,
	// keeping each one take accepts and blanking what was read.
	args := func(start, from, to int, take func(a string) (stop, whole bool)) (whole bool) {
		end, n := from, 0
		for _, f := range fieldsAt(p[from:to]) {
			stop, w := take(arg(f.s))
			if w {
				return true
			}
			if stop {
				break
			}
			end, n = from+f.end, n+1
		}
		if n == 0 {
			return true
		}
		blank(start, end)
		return false
	}
	whole := false
	for _, m := range goTestArgsRe.FindAllStringSubmatchIndex(p, -1) {
		whole = whole || args(m[0], m[2], m[3], func(a string) (bool, bool) {
			if strings.HasPrefix(a, "-") || a == "./..." || a == "..." || a == "." || a == "./" {
				return false, true
			}
			if !strings.HasPrefix(a, "./") || !insidePath(a) {
				return true, false
			}
			names = append(names, strings.TrimSuffix(a, "/"))
			return false, false
		})
	}
	for _, re := range []*regexp.Regexp{unittestArgsRe, pytestArgsRe} {
		for _, m := range re.FindAllStringSubmatchIndex(p, -1) {
			whole = whole || args(m[0], m[2], m[3], func(a string) (bool, bool) {
				if strings.HasPrefix(a, "-") || a == "discover" {
					return false, true
				}
				t := promptTestName(a)
				if t == "" {
					return true, false
				}
				names = append(names, t)
				return false, false
			})
		}
	}
	for _, m := range runTargetRe.FindAllStringSubmatchIndex(p, -1) {
		if t := promptTestName(arg(p[m[2]:m[3]])); t != "" {
			names = append(names, t)
			blank(m[0], m[3])
		}
	}
	if whole || len(names) == 0 {
		return nil, false
	}
	// What is left may say "the tests" or "run the tests", nothing more.
	left := plainTestsRe.ReplaceAllString(strings.ToLower(string(rest)), " ")
	if otherTestsRe.MatchString(left) {
		return nil, false
	}
	return names, true
}

var (
	// plainTestsRe is "the tests" with nothing qualifying it.
	plainTestsRe = regexp.MustCompile(`(?:\b(?:run|rerun|re-run)\s+)?\bthe\s+tests?\b`)
	// otherTestsRe is any other mention of tests, a runner or CI, or "also"
	// and "too", left in a message once its runner commands are taken out.
	otherTestsRe = regexp.MustCompile(`test|spec|suite|\bci\b|e2e|end-to-end|integration|\b(?:npm|pnpm|yarn|bun|make|gmake|cargo|tox|nox|jest|vitest|mocha|rspec|phpunit|ctest|mvn|gradle|gradlew|dotnet|bazel|unittest|pytest)\b|\balso\b|\btoo\b|as well`)
)

// wordAt is a whitespace-separated word and where it ends in its text.
type wordAt struct {
	s   string
	end int
}

func fieldsAt(s string) []wordAt {
	var out []wordAt
	start := -1
	for k, r := range s {
		space := r == ' ' || r == '\t' || r == '\n' || r == '\r'
		switch {
		case !space && start < 0:
			start = k
		case space && start >= 0:
			out = append(out, wordAt{s[start:k], k})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, wordAt{s[start:], len(s)})
	}
	return out
}

// promptTestName is a test module or file a message gives a runner, as a
// workspace path; "" for any other word.
func promptTestName(a string) string {
	if m := testModule(a); m != "" {
		return m
	}
	a = strings.TrimSuffix(strings.TrimPrefix(a, "./"), "/")
	if insidePath(a) && testName(a) && (strings.Contains(a, "/") || strings.HasSuffix(a, ".py")) {
		return a
	}
	return ""
}

var (
	goNoTestsRe  = regexp.MustCompile(`\[no test files\]|\[no tests to run\]|testing: warning: no tests to run`)
	goRanTestsRe = regexp.MustCompile(`(?m)^(?:ok\s+\S+\s+(?:[\d.]+s|\(cached\))(?:\s+coverage:[^\n]*)?\s*$|--- PASS|PASS$)|"Action":"pass","Package":"[^"]*","Test":"`)
	utNoTestsRe  = regexp.MustCompile(`\bRan 0 tests?\b|\bNO TESTS RAN\b`)
	utSummaryRe  = regexp.MustCompile(`\bRan (\d+) tests? in [^\n]*\n+\s*OK \(([^)\n]*)\)`)
	utNotRunRe   = regexp.MustCompile(`\b(?:skipped|expected failures)=(\d+)`)
	pyPassedRe   = regexp.MustCompile(`\b\d+ passed\b`)
	pyNoneRe     = regexp.MustCompile(`\bno tests ran\b|\b\d+ (?:skipped|deselected|xfailed|xpassed)\b`)
	cargoOKRe    = regexp.MustCompile(`test result: ok\. [1-9]\d* passed`)
)

// ranNothing reports a test run that passed without running a test: go
// test with no test files or none matched, unittest's "Ran 0 tests" or every
// test skipped, pytest with only skipped or deselected tests, cargo with
// none passed. It shows no test passing.
func ranNothing(o *obsRec) bool {
	if len(o.line.final) == 0 {
		return false
	}
	out := o.content
	switch prog := filepath.Base(o.line.final[0]); {
	case prog == "go":
		return goNoTestsRe.MatchString(out) && !goRanTestsRe.MatchString(out)
	case prog == "cargo":
		return strings.Contains(out, "test result:") && !cargoOKRe.MatchString(out)
	case prog == "pytest" || len(o.line.final) > 2 && o.line.final[2] == "pytest":
		return !pyPassedRe.MatchString(out) && pyNoneRe.MatchString(out)
	case len(o.line.final) > 2 && o.line.final[2] == "unittest":
		if utNoTestsRe.MatchString(out) {
			return true
		}
		// Every test skipped or an expected failure: none passed.
		m := utSummaryRe.FindStringSubmatch(out)
		if m == nil {
			return false
		}
		notRun := 0
		for _, n := range utNotRunRe.FindAllStringSubmatch(m[2], -1) {
			k, _ := strconv.Atoi(n[1])
			notRun += k
		}
		ran, _ := strconv.Atoi(m[1])
		return notRun > 0 && notRun >= ran
	}
	return false
}

// packageShadows reports a unittest run given a dotted name part of which
// is a package (tests.test_api, with tests/test_api/__init__.py): Python
// imports the package, not tests/test_api.py, so the run is not read as
// covering the file.
func (ix *recordIndex) packageShadows(words []string, dir string) bool {
	u, ok := parseUnittest(words)
	if !ok || ix.readFile == nil {
		return false
	}
	for _, n := range u.names {
		if strings.Contains(n, "/") || strings.HasSuffix(n, ".py") {
			continue
		}
		// Any part past the first that is a package (tests.test_api, with
		// tests/test_api/__init__.py), or a lone name that is one, is what
		// Python imports.
		parts := strings.Split(n, ".")
		for i := min(2, len(parts)); i <= len(parts); i++ {
			if _, err := ix.readFile(filepath.Join(dir, filepath.Join(parts[:i]...), "__init__.py")); err == nil {
				return true
			}
		}
	}
	return false
}

// goPackageUntested is the first folder a scoped claim names whose own
// package a go test run does not show passing a test: its result line is
// "?" ([no test files]), says [no tests to run], or is missing; "" when
// each has an ok line of its own (in -json output, a passing test). A
// claim of everything beneath a folder needs one such package under it.
// Only go test runs are read; any other is "".
func goPackageUntested(o *obsRec, scope []string) string {
	if len(o.line.final) < 2 || filepath.Base(o.line.final[0]) != "go" || len(scope) == 0 {
		return ""
	}
	dir := filepath.Clean(o.line.finalCwd)
	passed := goPassedPackages(o.content)
	for _, c := range scope {
		c, rec := recursiveScope(c)
		rel, err := filepath.Rel(dir, c)
		if err != nil || rel == "." && !rec || strings.HasPrefix(rel, "..") {
			continue // the run's own folder: judged as the whole run
		}
		rel = filepath.ToSlash(rel)
		found := false
		for _, pkg := range passed {
			if strings.HasSuffix(pkg, "/"+rel) || pkg == rel || rec && (rel == "." || strings.Contains(pkg+"/", "/"+rel+"/")) {
				found = true
				break
			}
		}
		if !found {
			return filepath.ToSlash(c)
		}
	}
	return ""
}

// goPassedPackages are the packages a go test run's output shows passing
// at least one test: an "ok" line without [no tests to run], or in -json
// output a test's pass.
func goPassedPackages(out string) []string {
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "{") {
			var ev struct{ Action, Package, Test string }
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Action == "pass" && ev.Test != "" && ev.Package != "" {
				pkgs = append(pkgs, ev.Package)
			}
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "ok" && !strings.Contains(line, "[no tests to run]") {
			pkgs = append(pkgs, f[1])
		}
	}
	return pkgs
}
