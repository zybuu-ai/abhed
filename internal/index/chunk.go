package index

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

// Chunking quality matters more than ranking quality: a hit that lands in the
// middle of a function is nearly useless to an agent, while a hit on the
// function boundary is immediately actionable.
//
// This splits on declaration boundaries using per-language patterns rather than
// a fixed window. That is deliberately lighter than a tree-sitter parse — it
// covers the languages an agent meets most, degrades to windowing for the
// rest, and adds no CGo dependency to an air-gapped build.

const (
	maxChunkLines = 120
	minChunkLines = 3
	overlapLines  = 3
)

type declPattern struct {
	re   *regexp.Regexp
	name int // capture group holding the symbol name
}

var languagePatterns = map[string][]declPattern{
	".go": {
		{regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`), 1},
		{regexp.MustCompile(`^type\s+([A-Za-z_]\w*)`), 1},
	},
	".py": {
		{regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_]\w*)`), 1},
		{regexp.MustCompile(`^class\s+([A-Za-z_]\w*)`), 1},
	},
	".js": jsPatterns, ".jsx": jsPatterns, ".ts": jsPatterns, ".tsx": jsPatterns,
	".java": {
		{regexp.MustCompile(`^\s*(?:public|private|protected)?\s*(?:static\s+)?(?:final\s+)?(?:class|interface|enum)\s+([A-Za-z_]\w*)`), 1},
		{regexp.MustCompile(`^\s*(?:public|private|protected)\s+(?:static\s+)?[\w<>\[\],\s]+\s+([A-Za-z_]\w*)\s*\(`), 1},
	},
	".rs": {
		{regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_]\w*)`), 1},
		{regexp.MustCompile(`^\s*(?:pub\s+)?(?:struct|enum|trait|impl)\s+([A-Za-z_]\w*)`), 1},
	},
	".rb": {
		{regexp.MustCompile(`^\s*def\s+([A-Za-z_][\w?!]*)`), 1},
		{regexp.MustCompile(`^\s*(?:class|module)\s+([A-Za-z_]\w*)`), 1},
	},
	".c": cPatterns, ".h": cPatterns, ".cc": cPatterns, ".cpp": cPatterns, ".hpp": cPatterns,
	".cs": {
		{regexp.MustCompile(`^\s*(?:public|private|protected|internal)?\s*(?:static\s+)?(?:class|interface|struct|enum)\s+([A-Za-z_]\w*)`), 1},
	},
	".php": {
		{regexp.MustCompile(`^\s*(?:public|private|protected)?\s*function\s+([A-Za-z_]\w*)`), 1},
		{regexp.MustCompile(`^\s*(?:class|interface|trait)\s+([A-Za-z_]\w*)`), 1},
	},
	".swift": {
		{regexp.MustCompile(`^\s*(?:public|private|internal)?\s*func\s+([A-Za-z_]\w*)`), 1},
		{regexp.MustCompile(`^\s*(?:class|struct|enum|protocol|extension)\s+([A-Za-z_]\w*)`), 1},
	},
	".kt": {
		{regexp.MustCompile(`^\s*(?:public|private|internal)?\s*fun\s+([A-Za-z_]\w*)`), 1},
		{regexp.MustCompile(`^\s*(?:class|object|interface)\s+([A-Za-z_]\w*)`), 1},
	},
	".sh": {
		{regexp.MustCompile(`^\s*(?:function\s+)?([A-Za-z_]\w*)\s*\(\)\s*\{`), 1},
	},
	".sql": {
		{regexp.MustCompile(`(?i)^\s*create\s+(?:or\s+replace\s+)?(?:table|view|function|procedure)\s+([A-Za-z_]\w*)`), 1},
	},
}

var jsPatterns = []declPattern{
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s+([A-Za-z_$]\w*)`), 1},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_$]\w*)`), 1},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$]\w*)\s*[:=]\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$]\w*)\s*=>`), 1},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:type|interface)\s+([A-Za-z_$]\w*)`), 1},
}

var cPatterns = []declPattern{
	{regexp.MustCompile(`^[A-Za-z_][\w\s*]*\s+\**([A-Za-z_]\w*)\s*\([^;]*$`), 1},
	{regexp.MustCompile(`^\s*(?:typedef\s+)?(?:struct|enum|union)\s+([A-Za-z_]\w*)`), 1},
}

// markdownHeading splits prose on headings, which are its natural boundaries.
var markdownHeading = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)

// chunkFile splits a file into indexable chunks with symbol attribution.
func chunkFile(path, content string) []Doc {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(path))
	lines := strings.Split(content, "\n")

	switch ext {
	case ".md", ".rst", ".txt":
		return chunkProse(path, ext, lines)
	}
	if patterns, found := languagePatterns[ext]; found {
		return chunkCode(path, ext, lines, patterns)
	}
	return chunkWindow(path, ext, lines, "")
}

// chunkCode splits at declaration boundaries so each chunk is a whole unit.
func chunkCode(path, ext string, lines []string, patterns []declPattern) []Doc {
	type boundary struct {
		line   int
		symbol string
	}
	var boundaries []boundary

	for i, line := range lines {
		for _, p := range patterns {
			if m := p.re.FindStringSubmatch(line); m != nil && len(m) > p.name {
				boundaries = append(boundaries, boundary{line: i, symbol: m[p.name]})
				break
			}
		}
	}

	if len(boundaries) == 0 {
		return chunkWindow(path, ext, lines, "")
	}

	var docs []Doc
	// Anything before the first declaration (imports, package docs) is its own
	// chunk: it carries real signal about what a file depends on.
	if boundaries[0].line > minChunkLines {
		docs = append(docs, makeDoc(path, ext, "", 1, boundaries[0].line, lines[:boundaries[0].line]))
	}

	for i, b := range boundaries {
		end := len(lines)
		if i+1 < len(boundaries) {
			end = boundaries[i+1].line
		}
		if end-b.line > maxChunkLines {
			// A very large declaration is windowed, but every window keeps the
			// symbol name so hits stay attributable.
			for start := b.line; start < end; start += maxChunkLines - overlapLines {
				stop := min(start+maxChunkLines, end)
				docs = append(docs, makeDoc(path, ext, b.symbol, start+1, stop, lines[start:stop]))
			}
			continue
		}
		docs = append(docs, makeDoc(path, ext, b.symbol, b.line+1, end, lines[b.line:end]))
	}
	return docs
}

func chunkProse(path, ext string, lines []string) []Doc {
	var docs []Doc
	start, heading := 0, ""

	flush := func(end int) {
		if end-start >= 1 {
			docs = append(docs, makeDoc(path, ext, heading, start+1, end, lines[start:end]))
		}
	}
	for i, line := range lines {
		if m := markdownHeading.FindStringSubmatch(line); m != nil {
			if i > start {
				flush(i)
			}
			start, heading = i, strings.TrimSpace(m[2])
		} else if i-start >= maxChunkLines {
			flush(i)
			start = i
		}
	}
	flush(len(lines))
	return docs
}

func chunkWindow(path, ext string, lines []string, symbol string) []Doc {
	var docs []Doc
	for start := 0; start < len(lines); start += maxChunkLines - overlapLines {
		end := min(start+maxChunkLines, len(lines))
		if end-start < minChunkLines && start > 0 {
			break
		}
		docs = append(docs, makeDoc(path, ext, symbol, start+1, end, lines[start:end]))
	}
	return docs
}

func makeDoc(path, ext, symbol string, startLine, endLine int, lines []string) Doc {
	content := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(path + ":" + content))
	return Doc{
		ID:        hex.EncodeToString(sum[:8]),
		Path:      path,
		Language:  strings.TrimPrefix(ext, "."),
		Symbol:    symbol,
		StartLine: startLine,
		EndLine:   endLine,
		Content:   content,
	}
}
