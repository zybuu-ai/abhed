package tools

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// The grep tool's search. The walk is Go's own and decides which files are
// searched: vendored folders, Abhed's state, links, files a .gitignore
// excludes and files over the size limit are passed over before anything is
// read. The files left are read through the confined root and matched in
// parallel. When ripgrep is installed, one ripgrep run alongside the walk
// only narrows which of those files are worth reading, by a literal every
// match must contain; what reaches the model is always this package's own
// read and match, so the path rules, the read deny rules and redaction hold
// the same either way.

// grepFile is one file the walk chose. It is looked at (lstat) only when it
// is read, in its folder held open.
type grepFile struct {
	path string // absolute
	rel  string // under the searched folder, for the confined read
	info fs.FileInfo
}

// grepHit is what one file gave.
type grepHit struct {
	path  string
	count int
	lines []string
}

// grepSpec is how files are matched.
type grepSpec struct {
	re        *regexp.Regexp
	pre       *regexp.Regexp // finds the required literals; nil tests every line
	lits      []string
	fold      bool
	multiline bool
	content   bool // keep the matching lines, for output_mode content
	context   int
}

// newGrepSpec finds the literals every match of expr contains.
func newGrepSpec(re *regexp.Regexp, expr string, multiline, content bool, ctxLines int) grepSpec {
	sp := grepSpec{re: re, multiline: multiline, content: content, context: ctxLines}
	lits, fold := requiredLiterals(expr)
	if len(lits) == 0 {
		return sp
	}
	alts := make([]string, len(lits))
	for i, l := range lits {
		alts[i] = regexp.QuoteMeta(l)
	}
	pre := strings.Join(alts, "|")
	if fold {
		pre = "(?i:" + pre + ")"
	}
	if c, err := regexp.Compile(pre); err == nil {
		sp.pre, sp.lits, sp.fold = c, lits, fold
	}
	return sp
}

// requiredLiterals returns strings at least one of which every match of
// expr contains, and whether any is matched without case; none when there
// is no such set. A literal holding a newline is not offered: ripgrep reads
// a newline in a pattern as two patterns.
func requiredLiterals(expr string) ([]string, bool) {
	parsed, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, false
	}
	set := needed(parsed.Simplify())
	fold := false
	for _, l := range set {
		if l.s == "" || strings.ContainsRune(l.s, '\n') {
			return nil, false
		}
		fold = fold || l.fold
	}
	out := make([]string, len(set))
	for i, l := range set {
		out[i] = l.s
	}
	return out, fold
}

type needLit struct {
	s    string
	fold bool
}

// needed is a set of literals one of which every match of re contains; nil
// when none is known.
func needed(re *syntax.Regexp) []needLit {
	switch re.Op {
	case syntax.OpLiteral:
		return []needLit{{s: string(re.Rune), fold: re.Flags&syntax.FoldCase != 0}}
	case syntax.OpCapture, syntax.OpPlus:
		return needed(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return needed(re.Sub[0])
		}
	case syntax.OpConcat:
		var best []needLit
		for _, sub := range re.Sub {
			if set := needed(sub); set != nil && shortest(set) > shortest(best) {
				best = set
			}
		}
		return best
	case syntax.OpAlternate:
		var all []needLit
		for _, sub := range re.Sub {
			set := needed(sub)
			if set == nil {
				return nil
			}
			all = append(all, set...)
		}
		return all
	}
	return nil
}

func shortest(set []needLit) int {
	if len(set) == 0 {
		return 0
	}
	n := len(set[0].s)
	for _, l := range set[1:] {
		n = min(n, len(l.s))
	}
	return n
}

// grepWalk lists the files under root a search reads. Folders are read in
// parallel; each folder's files stay together.
func (s *Session) grepWalk(ctx context.Context, root string, within *Confined, state *StateSet, globMatch func(string) bool) ([]grepFile, error) {
	var (
		mu    sync.Mutex
		files []grepFile
		wg    sync.WaitGroup
		sem   = make(chan struct{}, min(runtime.GOMAXPROCS(0), 8))
	)
	var visit func(dir string, ignores ignoreStack)
	visit = func(dir string, ignores ignoreStack) {
		if ctx.Err() != nil {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return // an unreadable folder is skipped, not fatal to the search
		}
		rel, _ := filepath.Rel(root, dir)
		for _, e := range entries {
			if e.Name() == ".gitignore" && e.Type().IsRegular() {
				r := within.Entries()
				if data, err := r.Read(filepath.Join(rel, e.Name()), nil, maxGrepFileSize); err == nil {
					ignores = append(ignores[:len(ignores):len(ignores)], parseGitignore(filepath.ToSlash(dir), data))
				}
				r.Close()
				break
			}
		}
		var mine []grepFile
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			slash := filepath.ToSlash(path)
			if e.IsDir() {
				if skipDirs[e.Name()] || ignores.ignored(slash, true) || state.HasEntry(e) {
					continue
				}
				wg.Add(1)
				select {
				case sem <- struct{}{}:
					go func() {
						defer func() { <-sem; wg.Done() }()
						visit(path, ignores)
					}()
				default:
					visit(path, ignores)
					wg.Done()
				}
				continue
			}
			// Links are not followed: one could lead out of the workspace or
			// into Abhed's state. A file that is a state file by another name
			// (a hardlink) is refused by the read, which judges what it opened.
			// Only regular files are read: a pipe would hold the search.
			if !e.Type().IsRegular() || strings.EqualFold(e.Name(), StateDir) {
				continue
			}
			fileRel := filepath.Join(rel, e.Name())
			if globMatch != nil && !globMatch(filepath.ToSlash(fileRel)) {
				continue
			}
			if ignores.ignored(slash, false) {
				continue
			}
			mine = append(mine, grepFile{path: path, rel: fileRel})
		}
		mu.Lock()
		files = append(files, mine...)
		mu.Unlock()
	}
	wg.Add(1)
	visit(root, s.parentIgnores(root))
	wg.Done()
	wg.Wait()
	return files, ctx.Err()
}

// parentIgnores are the .gitignore files from the session root holding root
// down to root's parent, so a search of a subfolder skips what the
// repository's own ignores. Each is read as the file tools read.
func (s *Session) parentIgnores(root string) ignoreStack {
	var top string
	for _, r := range append([]string{s.Root}, s.allowedRoots()...) {
		if rel, err := filepath.Rel(r, root); err == nil && filepath.IsLocal(rel) {
			top = r
			break
		}
	}
	if top == "" {
		return nil
	}
	var dirs []string
	for d := filepath.Dir(root); ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == top || filepath.Dir(d) == d {
			break
		}
	}
	var out ignoreStack
	for i := len(dirs) - 1; i >= 0; i-- {
		p := filepath.Join(dirs[i], ".gitignore")
		if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() || info.Size() > maxGrepFileSize {
			continue
		}
		if data, err := s.readFile(p); err == nil {
			out = append(out, parseGitignore(filepath.ToSlash(dirs[i]), data))
		}
	}
	return out
}

// grepFiles reads and matches files in parallel and returns their hits in
// the order given.
func grepFiles(ctx context.Context, within *Confined, files []grepFile, sp grepSpec) []grepHit {
	hits := make([]grepHit, len(files))
	// A job is a run of files in one folder, which the reader keeps open.
	type job struct{ lo, hi int }
	jobs := make(chan job, 64)
	go func() {
		defer close(jobs)
		for lo := 0; lo < len(files); {
			dir, hi := filepath.Dir(files[lo].rel), lo+1
			for hi < len(files) && hi-lo < 256 && filepath.Dir(files[hi].rel) == dir {
				hi++
			}
			select {
			case jobs <- job{lo, hi}:
			case <-ctx.Done():
				return
			}
			lo = hi
		}
	}()
	workers := min(min(runtime.GOMAXPROCS(0), 8), max(1, len(files)))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := within.Entries()
			defer r.Close()
			for jb := range jobs {
				for i := jb.lo; i < jb.hi && ctx.Err() == nil; i++ {
					f := files[i]
					data, err := r.Read(f.rel, f.info, maxGrepFileSize)
					if err != nil || IsBinary(data) {
						continue
					}
					hits[i] = sp.match(f.path, data)
				}
			}
		}()
	}
	wg.Wait()
	return hits
}

// match counts what one file's content gives: matching lines, or matches
// when multiline, keeping the lines when content is wanted.
func (sp grepSpec) match(path string, data []byte) grepHit {
	hit := grepHit{path: path}
	if sp.pre != nil && !sp.pre.Match(data) {
		return hit
	}
	var lines []string
	lineList := func() []string {
		if lines == nil {
			lines = strings.Split(string(data), "\n")
		}
		return lines
	}
	if sp.multiline {
		ms := sp.re.FindAllIndex(data, -1)
		hit.count = len(ms)
		if sp.content {
			for _, m := range ms {
				ln := bytes.Count(data[:m[0]], []byte("\n")) + 1
				hit.lines = append(hit.lines, formatMatch(lineList(), ln, sp.context))
				if len(hit.lines) >= maxGrepPerFile {
					break
				}
			}
		}
		return hit
	}
	// Every matching line holds one of the literals, so only lines that hold
	// one are tried; without literals, every line is.
	try := func(start, end, lineNo int) {
		if !sp.re.Match(data[start:end]) {
			return
		}
		hit.count++
		if sp.content && len(hit.lines) < maxGrepPerFile {
			hit.lines = append(hit.lines, formatMatch(lineList(), lineNo, sp.context))
		}
	}
	if sp.pre == nil {
		lineNo, start := 1, 0
		for start <= len(data) {
			end := bytes.IndexByte(data[start:], '\n')
			if end < 0 {
				end = len(data)
			} else {
				end += start
			}
			try(start, end, lineNo)
			start, lineNo = end+1, lineNo+1
		}
		return hit
	}
	lineNo, counted, lastLine := 1, 0, -1
	for _, m := range sp.pre.FindAllIndex(data, -1) {
		if m[0] < lastLine {
			continue // the line holding this literal was tried already
		}
		start := bytes.LastIndexByte(data[:m[0]], '\n') + 1
		end := bytes.IndexByte(data[m[0]:], '\n')
		if end < 0 {
			end = len(data)
		} else {
			end += m[0]
		}
		lineNo += bytes.Count(data[counted:start], []byte("\n"))
		counted = start
		try(start, end, lineNo)
		lastLine = end + 1
	}
	return hit
}

// ripgrep is the ripgrep program found on PATH when the process started, as
// an absolute path with its links resolved; "" when there is none. A PATH
// entry that is relative is never searched, so a workspace that is the
// current folder cannot supply it.
var ripgrep = findRipgrep()

func findRipgrep() string {
	p, err := exec.LookPath("rg")
	if err != nil || !filepath.IsAbs(p) {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return ""
}

// ripgrepFor is the ripgrep this session may run: none when it lies inside
// one of the session's roots, where the agent could have written it.
func (s *Session) ripgrepFor() string {
	if ripgrep == "" {
		return ""
	}
	for _, r := range append([]string{s.Root}, s.allowedRoots()...) {
		if within(ripgrep, []string{r, RealPath(r)}) {
			return ""
		}
	}
	return ripgrep
}

// ripgrepRun is one ripgrep search of a folder, started beside the walk.
type ripgrepRun struct {
	done  chan struct{}
	found map[string]bool
	ok    bool
}

// startRipgrep searches root with ripgrep for the files holding one of sp's
// literals. Only the literals reach ripgrep, as fixed strings, so its
// regular-expression dialect never decides a match; nil when ripgrep cannot
// be used for this pattern.
func startRipgrep(ctx context.Context, rg, root string, sp grepSpec) *ripgrepRun {
	if rg == "" || len(sp.lits) == 0 {
		return nil
	}
	for _, l := range sp.lits {
		// Case folding beyond ASCII is left to Go, whose tables decide the match.
		if sp.fold && !isASCII(l) || !utf8.ValidString(l) {
			return nil
		}
	}
	// The files it may search are a superset of the walk's: every file, as
	// text, hidden ones too, pruned only by .gitignore files in and below
	// root, as the walk prunes. Links are not followed.
	args := []string{"--no-config", "--files-with-matches", "--null", "--no-messages",
		"--fixed-strings", "--text", "--hidden", "--no-follow", "--no-require-git",
		"--no-ignore-global", "--no-ignore-exclude", "--no-ignore-dot", "--no-ignore-parent",
		"--max-filesize", strconv.Itoa(maxGrepFileSize)}
	if sp.fold {
		args = append(args, "--ignore-case")
	}
	// Folders the walk passes over need not be read, unless root is in one.
	inSkipped := false
	for _, part := range strings.Split(filepath.ToSlash(root), "/") {
		inSkipped = inSkipped || skipDirs[part] || strings.EqualFold(part, StateDir)
	}
	if !inSkipped {
		names := make([]string, 0, len(skipDirs)+1)
		for d := range skipDirs {
			names = append(names, d)
		}
		sort.Strings(names)
		for _, d := range append(names, StateDir) {
			args = append(args, "--glob", "!"+d+"/")
		}
	}
	for _, l := range sp.lits {
		args = append(args, "-e", l)
	}
	args = append(args, "--", root)
	run := &ripgrepRun{done: make(chan struct{})}
	go func() {
		defer close(run.done)
		cmd := exec.CommandContext(ctx, rg, args...) // #nosec G204 -- an absolute program found at start-up, run with an argument list
		// Nothing from the environment configures it, so no setting can add
		// a preprocessor or another program to the run.
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
		out, err := cmd.Output()
		var exit *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			// No file holds a literal.
		default:
			return // not judged: every walked file is read and matched
		}
		run.found = map[string]bool{}
		for _, p := range bytes.Split(out, []byte{0}) {
			if len(p) > 0 {
				run.found[string(p)] = true
			}
		}
		run.ok = true
	}()
	return run
}

// narrow keeps the walked files ripgrep found a literal in; all of them
// when ripgrep did not finish.
func (run *ripgrepRun) narrow(files []grepFile) []grepFile {
	if run == nil {
		return files
	}
	<-run.done
	if !run.ok {
		return files
	}
	var out []grepFile
	for _, f := range files {
		if run.found[f.path] {
			out = append(out, f)
		}
	}
	return out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// readCheckKey carries the read rules a tool's results are held to.
type readCheckKey struct{}

// WithReadCheck gives a tool run the session's read rules: withheld reports
// whether a read rule (a deny rule, or an ask rule a search cannot put to
// anyone) keeps the file at an absolute path from the agent. A search leaves
// out every file it withholds, so a match in a file the agent may not read
// never reaches the model.
func WithReadCheck(ctx context.Context, withheld func(path string) bool) context.Context {
	if withheld == nil {
		return ctx
	}
	return context.WithValue(ctx, readCheckKey{}, withheld)
}

// ReadRefused reports whether ctx's read rules withhold the file at path
// from a tool's results.
func ReadRefused(ctx context.Context, path string) bool {
	withheld, ok := ctx.Value(readCheckKey{}).(func(string) bool)
	return ok && withheld(path)
}
