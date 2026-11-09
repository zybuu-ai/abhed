package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Directories skipped by glob and grep unless explicitly targeted. These are
// where agents waste the most context: vendored code and build output.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "__pycache__": true, ".venv": true,
	"venv": true, ".next": true, ".cache": true, ".idea": true,
	".pytest_cache": true, ".mypy_cache": true, "coverage": true,
	WorktreesDir: true,
}

// SkipDir reports whether glob and grep pass over a directory of this name.
func SkipDir(name string) bool { return skipDirs[name] }

const (
	maxGlobResults  = 1000
	maxGrepMatches  = 100
	maxGrepPerFile  = 50
	maxGrepFileSize = 5 << 20 // 5 MB
)

// ---------------------------------------------------------------- glob

type Glob struct{}

func (Glob) Name() string  { return "glob" }
func (Glob) Mutates() bool { return false }

func (Glob) Description() string {
	return "Find files matching a glob pattern. Returns paths sorted by modification time, newest first."
}

func (Glob) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"Glob, e.g. **/*.ts or src/**/*_test.go"},
    "path":{"type":"string","description":"Directory to search from. Defaults to the workspace root."}
  },
  "required":["pattern"]
}`)
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (Glob) Run(ctx context.Context, s *Session, raw json.RawMessage) Result {
	var a globArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for glob: %v", err)
	}
	if a.Pattern == "" {
		return errf("pattern is required.")
	}

	root := s.Root
	if a.Path != "" {
		resolved, err := s.Resolve(a.Path)
		if err != nil {
			return errf("%v", err)
		}
		root = resolved
	}

	matcher, err := compileGlob(a.Pattern, FoldsCaseAt(root))
	if err != nil {
		return errf("Invalid glob pattern %q: %v", a.Pattern, err)
	}

	state := s.stateSet()
	type hit struct {
		path string
		mod  int64
	}
	var hits []hit

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || state.HasEntry(d)) {
				return filepath.SkipDir
			}
			return nil
		}
		// A link is listed only if a read of it would be allowed: it may lead
		// out of the workspace or into Abhed's state.
		if d.Type()&fs.ModeSymlink != 0 {
			if _, err := s.resolveWith(path, state); err != nil {
				return nil //nolint:nilerr // a link out of bounds is left out, not an error
			}
		} else if state.HasEntry(d) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}
		if matcher(filepath.ToSlash(rel)) {
			info, err := d.Info()
			var mod int64
			if err == nil {
				mod = info.ModTime().UnixNano()
			}
			hits = append(hits, hit{path: path, mod: mod})
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return errf("glob cancelled.")
	}

	if len(hits) == 0 {
		// A pattern into a skipped folder matches nothing by design; say so,
		// or the model concludes the folder is empty.
		return ok("[no files matched: %s%s]", a.Pattern, skippedFolderHint(a.Pattern, root))
	}

	// Newest first: recently touched files are usually the relevant ones.
	sort.Slice(hits, func(i, j int) bool { return hits[i].mod > hits[j].mod })

	truncated := false
	if len(hits) > maxGlobResults {
		hits = hits[:maxGlobResults]
		truncated = true
	}

	var b strings.Builder
	for _, h := range hits {
		b.WriteString(s.Rel(h.path))
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "\n[truncated at %d results. Narrow the pattern or set a subdirectory path.]", maxGlobResults)
	}
	return Result{Content: strings.TrimSuffix(b.String(), "\n"), Truncated: truncated}
}

// skippedFolderHint explains a no-match whose pattern names a folder glob passes over.
func skippedFolderHint(pattern, root string) string {
	segs := strings.Split(strings.TrimPrefix(filepath.ToSlash(pattern), "./"), "/")
	literal := true
	for i, seg := range segs {
		if strings.ContainsAny(seg, "*?") {
			literal = false
			continue
		}
		if !skipDirs[seg] {
			continue
		}
		if !literal {
			return fmt.Sprintf(". %s folders are skipped; to search one, pass its path", seg)
		}
		// path already is that folder: the pattern names it twice, and the walk would never find it.
		if i == 0 && filepath.Base(root) == seg {
			return ""
		}
		rest := strings.Join(segs[i+1:], "/")
		if rest == "" {
			rest = "**"
		}
		return fmt.Sprintf(". %s is skipped unless you pass path, such as path %q with pattern %q",
			seg, filepath.Join(append([]string{root}, segs[:i+1]...)...), rest)
	}
	return ""
}

// compileGlob translates a glob into a matcher. Supports ** (any depth),
// * (within a segment), and ? (single char) — the subset models actually use.
// fold matches without case, as a disk that folds case opens names.
func compileGlob(pattern string, fold bool) (func(string) bool, error) {
	p := filepath.ToSlash(pattern)
	var re strings.Builder
	if fold {
		re.WriteString("(?i)")
	}
	re.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				// "**/" matches zero or more leading segments.
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					re.WriteString("(?:.*/)?")
				} else {
					re.WriteString(".*")
				}
			} else {
				re.WriteString("[^/]*")
			}
		case '?':
			re.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			re.WriteByte('\\')
			re.WriteByte(p[i])
		default:
			re.WriteByte(p[i])
		}
	}
	re.WriteString("$")

	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return nil, err
	}
	return func(s string) bool {
		if compiled.MatchString(s) {
			return true
		}
		// A bare pattern like "*.go" should also match files in subdirectories,
		// which is what models expect when they write it.
		if !strings.Contains(p, "/") {
			return compiled.MatchString(filepath.Base(s))
		}
		return false
	}, nil
}

// ---------------------------------------------------------------- grep

type Grep struct{}

func (Grep) Name() string  { return "grep" }
func (Grep) Mutates() bool { return false }

func (Grep) Description() string {
	return "Search file contents with a regular expression (RE2 syntax). This is the primary code-navigation tool — prefer it over reading files speculatively."
}

func (Grep) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"Regular expression (RE2 syntax; no lookahead or backreferences)."},
    "path":{"type":"string","description":"File or directory to search. Defaults to the workspace root."},
    "glob":{"type":"string","description":"Filter files by glob, e.g. *.go"},
    "output_mode":{"type":"string","enum":["content","files_with_matches","count"],"description":"Default files_with_matches."},
    "context":{"type":"integer","description":"Lines of context around each match (content mode only)."},
    "case_insensitive":{"type":"boolean"},
    "multiline":{"type":"boolean","description":"Allow . to match newlines."}
  },
  "required":["pattern"]
}`)
}

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	OutputMode      string `json:"output_mode"`
	Context         int    `json:"context"`
	CaseInsensitive bool   `json:"case_insensitive"`
	Multiline       bool   `json:"multiline"`
}

func (Grep) Run(ctx context.Context, s *Session, raw json.RawMessage) Result {
	var a grepArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for grep: %v", err)
	}
	if a.Pattern == "" {
		return errf("pattern is required.")
	}

	expr := a.Pattern
	var flags string
	if a.CaseInsensitive {
		flags += "i"
	}
	if a.Multiline {
		flags += "s"
	}
	if flags != "" {
		expr = "(?" + flags + ")" + expr
	}

	// RE2: no backtracking, so no catastrophic-regex DoS from model output.
	// Models routinely try lookaheads, so name the problem when they do.
	re, err := regexp.Compile(expr)
	if err != nil {
		hint := ""
		msg := err.Error()
		switch {
		case strings.Contains(msg, "invalid or unsupported Perl syntax"):
			hint = " Abhed uses RE2: lookahead (?=), lookbehind (?<=) and backreferences (\\1) are unsupported. Rewrite the pattern without them, or use two greps."
		case strings.Contains(msg, "missing closing"):
			hint = " Check for an unbalanced bracket or parenthesis."
		}
		return errf("Invalid pattern %q: %v.%s", a.Pattern, err, hint)
	}

	root := s.Root
	// A glob in path, as models often write it, is split into the folder it
	// starts from and the glob below it.
	if base, pattern, isGlob := splitPathGlob(a.Path, func(p string) bool {
		path, err := s.Resolve(p)
		if err != nil {
			return false
		}
		_, err = os.Lstat(path)
		return err == nil
	}); isGlob {
		if a.Glob != "" {
			return errf("path %q is a glob and glob is set too. Put the folder in path and the pattern in glob, e.g. path %q, glob %q.", a.Path, base, pattern)
		}
		a.Path, a.Glob = base, pattern
	}
	if a.Path != "" {
		resolved, err := s.Resolve(a.Path)
		if err != nil {
			return errf("%v", err)
		}
		root = resolved
	}

	var globMatch func(string) bool
	if a.Glob != "" {
		globMatch, err = compileGlob(a.Glob, FoldsCaseAt(root))
		if err != nil {
			return errf("Invalid glob %q: %v", a.Glob, err)
		}
	}

	mode := a.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}

	sp := newGrepSpec(re, expr, a.Multiline, mode == "content", a.Context)
	info, statErr := os.Stat(root)
	if statErr != nil {
		return errf("Cannot access %s: %v", a.Path, statErr)
	}
	var hits []grepHit
	withheld := 0
	if info.IsDir() {
		state := s.stateSet()
		// Files are read under the searched folder held open as a root, so a
		// folder swapped for a link during the walk cannot lead the read out.
		within, err := state.Confine(root)
		if err != nil {
			return errf("Cannot search %s: %v", root, err)
		}
		defer within.Close()
		rg := startRipgrep(ctx, s.ripgrepFor(), root, sp)
		var files []grepFile
		files, withheld, _ = s.grepWalk(ctx, root, within, state, globMatch)
		hits = grepFiles(ctx, within, rg.narrow(files), sp)
	} else if ReadRefused(ctx, root) {
		// Named outright, a withheld file is answered as read answers it.
		return errf("%s is withheld by a read rule, so grep does not search it.", a.Path)
	} else if globMatch == nil || globMatch(filepath.Base(root)) {
		// A file named as the path is searched as named; a link there was
		// already resolved.
		if data, err := s.readFile(root); err == nil && info.Size() <= maxGrepFileSize && !IsBinary(data) {
			hits = []grepHit{sp.match(root, data)}
		}
	}

	var results []grepHit
	total := 0
	for _, h := range hits {
		if h.count == 0 {
			continue
		}
		results = append(results, h)
		total += h.count
	}
	// Counted from paths in the walk, before any read: the same whatever the
	// pattern, so it says nothing of what those files hold.
	note := ""
	if withheld > 0 {
		note = fmt.Sprintf("\n[%d file(s) were not searched because a read rule withholds them]", withheld)
	}

	if ctx.Err() != nil {
		return errf("grep cancelled.")
	}
	if len(results) == 0 {
		where := ""
		if a.Path != "" {
			where = " in " + a.Path
		}
		return ok("No matches for %s%s. Try a broader pattern, or check the path.%s", a.Pattern, where, note)
	}

	// Most matches first; the path breaks ties, so a parallel search reads the same each time.
	sort.Slice(results, func(i, j int) bool {
		if results[i].count != results[j].count {
			return results[i].count > results[j].count
		}
		return results[i].path < results[j].path
	})

	var b strings.Builder
	truncated := false
	switch mode {
	case "count":
		for _, r := range results {
			fmt.Fprintf(&b, "%s: %d\n", s.Rel(r.path), r.count)
		}
		fmt.Fprintf(&b, "\n%d matches across %d files.", total, len(results))
	case "content":
		shown := 0
		for _, r := range results {
			fmt.Fprintf(&b, "\n%s\n", s.Rel(r.path))
			for _, l := range r.lines {
				b.WriteString(l)
				b.WriteByte('\n')
				shown++
				if shown >= maxGrepMatches {
					truncated = true
					break
				}
			}
			if truncated {
				break
			}
		}
		if truncated {
			fmt.Fprintf(&b, "\n[truncated at %d matches of %d total. Narrow the pattern or set a path.]", maxGrepMatches, total)
		}
	default: // files_with_matches
		for _, r := range results {
			fmt.Fprintf(&b, "%s (%d)\n", s.Rel(r.path), r.count)
		}
		fmt.Fprintf(&b, "\n%d files, %d matches. Use output_mode:\"content\" to see them.", len(results), total)
	}

	return Result{Content: strings.TrimSpace(b.String()) + note, Truncated: truncated}
}

func formatMatch(lines []string, lineNo, ctxLines int) string {
	if ctxLines <= 0 {
		return fmt.Sprintf("%6d\t%s", lineNo, truncateLine(lines[lineNo-1]))
	}
	start := lineNo - ctxLines
	if start < 1 {
		start = 1
	}
	end := lineNo + ctxLines
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		sep := "-"
		if i == lineNo {
			sep = ":"
		}
		fmt.Fprintf(&b, "%6d%s\t%s\n", i, sep, truncateLine(lines[i-1]))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func truncateLine(s string) string {
	if len(s) > 300 {
		return s[:300] + " [...]"
	}
	return s
}

// splitPathGlob splits a path holding * or ? (the glob characters
// compileGlob reads) into the folder before the first segment that has one
// ("" for the root) and the rest. A path that exists as written, as exists
// says (inside the session only), such as a folder named a*b, is never split.
func splitPathGlob(p string, exists func(string) bool) (string, string, bool) {
	if !strings.ContainsAny(p, "*?") || exists(p) {
		return "", "", false
	}
	segs := strings.Split(filepath.ToSlash(p), "/")
	for i, seg := range segs {
		if strings.ContainsAny(seg, "*?") {
			base := strings.Join(segs[:i], "/")
			if base == "" && strings.HasPrefix(p, "/") {
				base = "/"
			}
			return filepath.FromSlash(base), strings.Join(segs[i:], "/"), true
		}
	}
	return "", "", false
}
