package tools

import (
	"regexp"
	"strings"
)

// ignoreFile is one .gitignore's rules, for paths below dir (slash-separated, absolute).
type ignoreFile struct {
	dir   string
	rules []ignoreRule
}

type ignoreRule struct {
	re      *regexp.Regexp // nil when name decides
	name    string         // a pattern with no slash or glob matches this base name exactly
	base    bool           // re is matched against the base name only
	negate  bool
	dirOnly bool
}

// matches reports whether the rule matches rel, a path under its file's folder.
func (r ignoreRule) matches(rel string) bool {
	if r.re == nil || r.base {
		b := rel
		if i := strings.LastIndexByte(rel, '/'); i >= 0 {
			b = rel[i+1:]
		}
		if r.re == nil {
			return b == r.name
		}
		return r.re.MatchString(b)
	}
	return r.re.MatchString(rel)
}

// parseGitignore reads a .gitignore's lines as git does: comments, blank
// lines, ! to re-include, a trailing / for folders only, and a slash before
// the end anchoring the pattern to the file's folder. A line that does not
// compile is left out.
func parseGitignore(dir string, data []byte) ignoreFile {
	f := ignoreFile{dir: dir}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = trimIgnoreSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		if strings.HasPrefix(line, "!") {
			r.negate, line = true, line[1:]
		} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly, line = true, strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		// A name with no slash matches at any depth, which is the base name
		// alone; with no glob either, it is a plain comparison.
		if !anchored && !strings.ContainsAny(line, `*?[\`) {
			r.name = line
			f.rules = append(f.rules, r)
			continue
		}
		expr := "^" + ignoreRegexp(line) + "$"
		r.base = !anchored && !strings.Contains(line, "**")
		if !anchored && !r.base {
			expr = "^(?:.*/)?" + ignoreRegexp(line) + "$"
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			continue
		}
		r.re = re
		f.rules = append(f.rules, r)
	}
	return f
}

// trimIgnoreSpace drops trailing spaces unless the last is escaped.
func trimIgnoreSpace(s string) string {
	for strings.HasSuffix(s, " ") && !strings.HasSuffix(s, `\ `) {
		s = s[:len(s)-1]
	}
	return s
}

// ignoreRegexp translates one gitignore glob into a regular expression.
func ignoreRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			atStart := i == 1
			slashAfter := i+1 < len(p) && p[i+1] == '/'
			switch {
			case slashAfter && (atStart || p[i-2] == '/'):
				i++ // **/ is zero or more folders
				b.WriteString("(?:.*/)?")
			default:
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := p[i+1 : i+1+end]
			i += end + 1
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
		case c == '\\' && i+1 < len(p):
			i++
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// ignoreStack holds the .gitignore files that apply at one point of a walk,
// outermost first.
type ignoreStack []ignoreFile

// ignored reports whether the entry at abs (slash-separated) is excluded:
// the last rule that matches decides, a deeper file's after its parents'.
func (st ignoreStack) ignored(abs string, isDir bool) bool {
	out := false
	for _, f := range st {
		rel, ok := strings.CutPrefix(abs, f.dir+"/")
		if !ok || rel == "" {
			continue
		}
		for _, r := range f.rules {
			if r.dirOnly && !isDir {
				continue
			}
			if r.matches(rel) {
				out = !r.negate
			}
		}
	}
	return out
}
