package agent

import (
	"path/filepath"
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
// never what runs or how it is judged. Any other variable set for a run
// (GOFLAGS=-run=X, PYTEST_ADDOPTS='-k x', CI, UPDATE) may filter it or change
// what passes, so the run is not taken as a whole one.
var colourEnv = map[string]bool{"NO_COLOR": true, "FORCE_COLOR": true, "CLICOLOR": true, "CLICOLOR_FORCE": true}

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
	case "pytest", "python", "python3":
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

// claimScope is where a claim's named packages or folders are, in the
// workspace; none names the whole of it.
func claimScope(scope []string, workDir string) []string {
	var out []string
	for _, s := range scope {
		s = strings.TrimSuffix(strings.TrimSuffix(slashes(s), "..."), "/")
		if s == "" || s == "." {
			continue
		}
		if !filepath.IsAbs(s) {
			s = filepath.Join(workDir, strings.TrimPrefix(s, "./"))
		}
		out = append(out, filepath.Clean(s))
	}
	return out
}

// coversScope reports a run that covers every folder a claim names, by path,
// or the whole workspace when it names none.
func coversScope(pats []scopePat, scope []string, workDir string) bool {
	covered := func(c string) bool {
		for _, p := range pats {
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
		if p.dir != scope[i] {
			return false
		}
	}
	return true
}
