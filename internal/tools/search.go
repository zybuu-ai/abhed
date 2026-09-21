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
}

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

	matcher, err := compileGlob(a.Pattern)
	if err != nil {
		return errf("Invalid glob pattern %q: %v", a.Pattern, err)
	}

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
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
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
		return ok("[no files matched: %s]", a.Pattern)
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

// compileGlob translates a glob into a matcher. Supports ** (any depth),
// * (within a segment), and ? (single char) — the subset models actually use.
func compileGlob(pattern string) (func(string) bool, error) {
	p := filepath.ToSlash(pattern)
	var re strings.Builder
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
	if a.Path != "" {
		resolved, err := s.Resolve(a.Path)
		if err != nil {
			return errf("%v", err)
		}
		root = resolved
	}

	var globMatch func(string) bool
	if a.Glob != "" {
		globMatch, err = compileGlob(a.Glob)
		if err != nil {
			return errf("Invalid glob %q: %v", a.Glob, err)
		}
	}

	mode := a.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}

	type fileHit struct {
		path  string
		count int
		lines []string
	}
	var results []fileHit
	total := 0

	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if globMatch != nil {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil || !globMatch(filepath.ToSlash(rel)) {
				return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
			}
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > maxGrepFileSize {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || IsBinary(data) {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}

		hit := fileHit{path: path}
		lines := strings.Split(string(data), "\n")

		if a.Multiline {
			if ms := re.FindAllStringIndex(string(data), -1); len(ms) > 0 {
				hit.count = len(ms)
				if mode == "content" {
					for _, m := range ms {
						ln := strings.Count(string(data[:m[0]]), "\n") + 1
						hit.lines = append(hit.lines, formatMatch(lines, ln, a.Context))
						if len(hit.lines) >= maxGrepPerFile {
							break
						}
					}
				}
			}
		} else {
			for i, line := range lines {
				if re.MatchString(line) {
					hit.count++
					if mode == "content" && len(hit.lines) < maxGrepPerFile {
						hit.lines = append(hit.lines, formatMatch(lines, i+1, a.Context))
					}
				}
			}
		}

		if hit.count > 0 {
			results = append(results, hit)
			total += hit.count
		}
		return nil
	}

	info, statErr := os.Stat(root)
	if statErr != nil {
		return errf("Cannot access %s: %v", a.Path, statErr)
	}
	if info.IsDir() {
		_ = filepath.WalkDir(root, walk)
	} else {
		_ = walk(root, fs.FileInfoToDirEntry(info), nil)
	}

	if ctx.Err() != nil {
		return errf("grep cancelled.")
	}
	if len(results) == 0 {
		where := ""
		if a.Path != "" {
			where = " in " + a.Path
		}
		return ok("No matches for %s%s. Try a broader pattern, or check the path.", a.Pattern, where)
	}

	sort.Slice(results, func(i, j int) bool { return results[i].count > results[j].count })

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

	return Result{Content: strings.TrimSpace(b.String()), Truncated: truncated}
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
