package server

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Searching the workspace from the workbench. It reads only what the Explorer
// would show: each folder and file is put to the view's rules before it is
// opened, so a read-denied path is never searched, and nothing is recorded,
// as nothing is changed. It passes over the folders the grep tool does, so a
// person and the agent searching for the same thing find the same files.

const (
	maxSearchQuery   = 1000
	maxSearchMatches = 2000
	maxSearchPerFile = 100
	maxSearchFiles   = 20000
	maxSearchPreview = 240
	searchTimeout    = 5 * time.Second
)

type searchMatch struct {
	Line int `json:"line"`
	// Col and End are 1-based columns in UTF-16 units, as the editor counts them.
	Col  int    `json:"col"`
	End  int    `json:"end"`
	Text string `json:"text"`
	// From and To mark the match within Text, in UTF-16 units.
	From int `json:"from"`
	To   int `json:"to"`
}

type searchFile struct {
	Path    string        `json:"path"`
	Matches []searchMatch `json:"matches"`
}

type searchResponse struct {
	Files     []searchFile `json:"files"`
	Matches   int          `json:"matches"`
	Truncated bool         `json:"truncated"`
	// Why says what cut the search short: matches, files or time.
	Why string `json:"why,omitempty"`
}

// searchPattern builds the expression from the query and the three toggles.
func searchPattern(q string, regex, matchCase, word bool) (*regexp.Regexp, error) {
	expr := q
	if !regex {
		expr = regexp.QuoteMeta(q)
	}
	if word {
		expr = `\b(?:` + expr + `)\b`
	}
	if !matchCase {
		expr = "(?i)" + expr
	}
	return regexp.Compile(expr)
}

// searchSession finds a query in the workspace's text files, bounded in
// matches, files and time.
func (s *Server) searchSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := q.Get("q")
	if query == "" {
		WriteError(w, http.StatusBadRequest, "q is required")
		return
	}
	if len(query) > maxSearchQuery {
		WriteError(w, http.StatusBadRequest, "the query is too long")
		return
	}
	on := func(k string) bool { return q.Get(k) == "1" || q.Get(k) == "true" }
	re, err := searchPattern(query, on("regex"), on("case"), on("word"))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "not a valid expression (RE2 syntax): "+err.Error())
		return
	}
	v, ok := s.view(w, r)
	if !ok {
		return
	}
	defer v.Close()

	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	out := searchResponse{Files: []searchFile{}}
	files := 0
	stop := func(why string) error {
		out.Truncated, out.Why = true, why
		return fs.SkipAll
	}
	_ = filepath.WalkDir(v.sess.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the search
		}
		if ctx.Err() != nil {
			return stop("time")
		}
		rel, rerr := filepath.Rel(v.sess.Root, p)
		if rerr != nil || rel == "." {
			return nil //nolint:nilerr // the root itself is not a match
		}
		if d.IsDir() {
			if viewerSkip[d.Name()] || tools.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			if _, err := v.resolve(rel); err != nil {
				return filepath.SkipDir
			}
			return nil
		}
		if files++; files > maxSearchFiles {
			return stop("files")
		}
		target, err := v.resolve(rel)
		if err != nil {
			return nil //nolint:nilerr // a path the view refuses is not searched
		}
		data, _, err := v.read(target)
		if err != nil || tools.IsBinary(data) {
			return nil //nolint:nilerr // unreadable and binary files are not searched
		}
		hit := searchLines(string(trimPartialRune(data)), re)
		if len(hit) == 0 {
			return nil
		}
		if room := maxSearchMatches - out.Matches; len(hit) > room {
			hit = hit[:room]
		}
		out.Files = append(out.Files, searchFile{Path: filepath.ToSlash(rel), Matches: hit})
		if out.Matches += len(hit); out.Matches >= maxSearchMatches {
			return stop("matches")
		}
		return nil
	})
	WriteJSON(w, http.StatusOK, out)
}

// searchLines returns the matches in one file's text, at most maxSearchPerFile.
func searchLines(text string, re *regexp.Regexp) []searchMatch {
	var out []searchMatch
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, m := range re.FindAllStringIndex(line, -1) {
			if m[0] == m[1] {
				continue // an empty match marks nothing
			}
			out = append(out, previewMatch(i+1, line, m[0], m[1]))
			if len(out) >= maxSearchPerFile {
				return out
			}
		}
	}
	return out
}

// previewMatch cuts a long line down to a window around the match.
func previewMatch(n int, line string, start, end int) searchMatch {
	m := searchMatch{Line: n, Col: utf16Len(line[:start]) + 1, End: utf16Len(line[:end]) + 1}
	from, to := 0, len(line)
	if len(line) > maxSearchPreview {
		from = max(0, start-maxSearchPreview/4)
		to = min(len(line), max(end, from+maxSearchPreview))
		for from > 0 && !utf8Start(line[from]) {
			from--
		}
		for to < len(line) && !utf8Start(line[to]) {
			to++
		}
	}
	m.Text = line[from:to]
	m.From = utf16Len(line[from:start])
	m.To = m.From + utf16Len(line[start:end])
	return m
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// utf16Len counts s as a JavaScript string would.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}
