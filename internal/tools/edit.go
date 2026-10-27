package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Edit replaces an exact string in a file.
//
// Exact matching is deliberate. Line numbers drift the moment anything above
// them changes, and models produce malformed unified diffs at a meaningful
// rate. Exact string matching fails loudly and safely: a non-match changes
// nothing, and the error tells the model precisely what to fix.
//
// Never fuzzy-match content. The one tolerance is indentation: when the text
// matches nowhere exactly but, line for line, matches exactly one place once
// leading whitespace is set aside, it is applied there, re-indented to the
// file's own indentation, and the result says it was matched loosely. Two
// such places refuse, as an exact double match does.
type Edit struct{}

func (Edit) Name() string  { return "edit" }
func (Edit) Mutates() bool { return true }

func (Edit) Description() string {
	return "Replace an exact string in a file. The old_string must match the file content exactly, including whitespace and indentation. Include enough surrounding context to make the match unique."
}

func (Edit) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"Absolute path, or relative to the workspace root."},
    "old_string":{"type":"string","description":"Exact text to replace, including indentation. Must be unique in the file unless replace_all is true."},
    "new_string":{"type":"string","description":"Replacement text. Must differ from old_string."},
    "replace_all":{"type":"boolean","description":"Replace every occurrence. Default false."}
  },
  "required":["path","old_string","new_string"]
}`)
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// Precheck refuses a path the edit could never use, before anyone is asked.
func (Edit) Precheck(s *Session, raw json.RawMessage) error { return precheckPath(s, raw) }

func (Edit) Run(ctx context.Context, s *Session, raw json.RawMessage) Result {
	var a editArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for edit: %v", err)
	}
	path, err := s.Resolve(a.Path)
	if err == nil {
		err = gitWrite(path)
	}
	if err != nil {
		return errf("%v", err)
	}
	if err := s.guard(path); err != nil {
		return errf("%s %v", NotApplied, err)
	}
	if a.OldString == a.NewString {
		return errf("old_string and new_string are identical — this edit would do nothing.")
	}
	diffNote := ""
	if s.Syntax != SyntaxOff && looksLikeDiff(a.Path, a.OldString, a.NewString) {
		const why = "new_string looks like a pasted diff: every line starts with + or -. Pass the new text itself, without diff markers."
		if s.Syntax == SyntaxRefuse {
			return errf("%s %s", NotApplied, why)
		}
		diffNote = "Warning: " + why
	}

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		// Empty old_string on a missing file is equivalent to a create.
		if a.OldString == "" {
			note, _ := s.syntaxVerdict(ctx, path, nil, false, []byte(a.NewString))
			if diffNote != "" {
				note = strings.TrimSpace(diffNote + "\n" + note)
			}
			s.recordChange(path)
			if err := s.atomicWrite(path, []byte(a.NewString), 0o644); err != nil {
				return errf("Create failed for %s: %v", a.Path, err)
			}
			s.MarkRead(path, a.NewString)
			return ok("Created %s (%d bytes).%s", s.Rel(path), len(a.NewString), suffix(note))
		}
		return errf("File not found: %s. Use glob to locate it, or write() to create it.", a.Path)
	}
	if err != nil {
		return errf("Cannot access %s: %v", a.Path, err)
	}
	if info.IsDir() {
		return errf("%s is a directory, not a file.", a.Path)
	}

	if !s.WasRead(path) {
		return errf("Refusing to edit %s — not read this session. Call read(%q) first.", a.Path, a.Path)
	}
	if s.ChangedSinceRead(path) {
		return errf("%s changed on disk since you read it. Re-read it before editing.", a.Path)
	}

	data, err := s.readFile(path)
	if err != nil {
		return errf("Cannot read %s: %v", a.Path, err)
	}
	content := string(data)

	if a.OldString == "" {
		return errf("old_string is empty and %s already exists. To replace the whole file, use write().", a.Path)
	}

	count := strings.Count(content, a.OldString)
	loose := ""
	if count == 0 && !a.ReplaceAll {
		updated, reindented, places, lines := indentMatch(content, a.OldString, a.NewString)
		switch {
		case places == 1:
			content, a.NewString, count = updated, reindented, 1
			loose = fmt.Sprintf("Matched loosely at line %d: the indentation differed from the file's, so the replacement was re-indented to match it.", lines[0])
		case places > 1:
			return errf("old_string matches nowhere exactly, and %d places once indentation is set aside (lines %s).\nCopy the exact text, including leading whitespace, with enough context to make it unique.",
				places, strings.Join(intsToStrings(lines), ", "))
		}
	}
	switch {
	case count == 0:
		return errf("%s", noMatchMessage(a.Path, a.OldString, content))
	case count > 1 && !a.ReplaceAll:
		return errf("old_string appears %d times in %s (lines %s).\nAdd surrounding context to make it unique, or set replace_all: true.",
			count, a.Path, strings.Join(matchLines(content, a.OldString), ", "))
	}

	var updated string
	switch {
	case loose != "":
		updated = content // already the file with the loose match applied
	case a.ReplaceAll:
		updated = strings.ReplaceAll(content, a.OldString, a.NewString)
	default:
		updated = strings.Replace(content, a.OldString, a.NewString, 1)
	}

	note, refuse := s.syntaxVerdict(ctx, path, data, true, []byte(updated))
	if loose != "" {
		note = strings.TrimSpace(loose + "\n" + note)
	}
	if refuse {
		return errf("%s", note)
	}
	if diffNote != "" {
		note = strings.TrimSpace(diffNote + "\n" + note)
	}

	mode := info.Mode().Perm()
	s.recordChange(path)
	if err := s.atomicWrite(path, []byte(updated), mode); err != nil {
		return errf("Write failed for %s: %v", a.Path, err)
	}
	s.MarkRead(path, updated)

	// Return the changed region so the model sees the result without re-reading.
	// Saves a turn and confirms the edit landed where intended.
	snippet := changedRegion(updated, a.NewString, 4)
	if a.ReplaceAll {
		return ok("Replaced %d occurrences in %s.\n%s%s", count, s.Rel(path), snippet, suffix(note))
	}
	return ok("Edited %s.\n%s%s", s.Rel(path), snippet, suffix(note))
}

// noMatchMessage builds the most important error in the tool set. A bare
// "not found" is a dead end; naming the nearest partial match turns it into a
// recoverable turn.
func noMatchMessage(path, old, content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "old_string not found in %s.\n", path)
	b.WriteString("The file may have changed, or whitespace/indentation may differ.\n")

	if line, snippet := nearestMatch(content, old); line > 0 {
		fmt.Fprintf(&b, "Nearest partial match at line %d:\n%s\n", line, snippet)
	}
	b.WriteString("Re-read the file and copy the exact text, including leading whitespace.")
	return b.String()
}

// nearestMatch finds the line whose content best overlaps the first line of
// old_string. Cheap heuristic, but it catches the common cases: changed
// indentation, a renamed identifier, a line that moved.
func nearestMatch(content, old string) (int, string) {
	firstLine := strings.TrimSpace(strings.SplitN(old, "\n", 2)[0])
	if firstLine == "" {
		return 0, ""
	}
	lines := strings.Split(content, "\n")

	bestLine, bestScore := 0, 0.0
	for i, line := range lines {
		score := similarity(strings.TrimSpace(line), firstLine)
		if score > bestScore {
			bestScore, bestLine = score, i+1
		}
	}
	// 0.3 keeps genuine near-misses (a changed signature, added parameter,
	// renamed variable) while rejecting unrelated lines. Tuned against the
	// conformance corpus; see docs/architecture/08-eval.md.
	if bestScore < 0.3 {
		return 0, ""
	}

	start := bestLine - 2
	if start < 1 {
		start = 1
	}
	end := bestLine + 2
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		marker := "  "
		if i == bestLine {
			marker = "> "
		}
		fmt.Fprintf(&b, "%s%6d\t%s\n", marker, i, lines[i-1])
	}
	return bestLine, strings.TrimSuffix(b.String(), "\n")
}

// similarity ranks how likely two lines are to be the "same" line. It blends
// token overlap with shared-prefix length: the common near-miss cases (an added
// parameter, a changed return type, different indentation) keep a long common
// prefix even when token overlap drops, so prefix carries real signal here.
func similarity(a, b string) float64 {
	if a == b {
		return 1.0
	}
	if a == "" || b == "" {
		return 0
	}
	jaccard := tokenOverlap(a, b)
	prefix := float64(commonPrefixLen(a, b))
	longest := float64(max(len(a), len(b)))
	var prefixScore float64
	if longest > 0 {
		prefixScore = prefix / longest
	}
	// Weighted toward prefix: a long shared prefix is stronger evidence of
	// "same line, edited" than raw token overlap.
	return 0.4*jaccard + 0.6*prefixScore
}

func commonPrefixLen(a, b string) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func tokenOverlap(a, b string) float64 {
	af, bf := strings.Fields(a), strings.Fields(b)
	if len(af) == 0 || len(bf) == 0 {
		// Fall back to character bigrams for single-token lines.
		af, bf = bigrams(a), bigrams(b)
		if len(af) == 0 || len(bf) == 0 {
			return 0
		}
	}
	set := make(map[string]int, len(af))
	for _, t := range af {
		set[t]++
	}
	shared := 0
	for _, t := range bf {
		if set[t] > 0 {
			set[t]--
			shared++
		}
	}
	total := len(af) + len(bf) - shared
	if total == 0 {
		return 0
	}
	return float64(shared) / float64(total)
}

func bigrams(s string) []string {
	if len(s) < 2 {
		return nil
	}
	out := make([]string, 0, len(s)-1)
	for i := 0; i < len(s)-1; i++ {
		out = append(out, s[i:i+2])
	}
	return out
}

// matchLines reports the 1-indexed line numbers where a substring starts.
func matchLines(content, sub string) []string {
	var out []string
	offset := 0
	for {
		idx := strings.Index(content[offset:], sub)
		if idx < 0 {
			break
		}
		abs := offset + idx
		out = append(out, fmt.Sprintf("%d", strings.Count(content[:abs], "\n")+1))
		offset = abs + len(sub)
		if len(out) >= 10 {
			out = append(out, "...")
			break
		}
	}
	return out
}

// changedRegion renders the replacement with surrounding context and line
// numbers, so the model can confirm placement.
func changedRegion(updated, needle string, ctx int) string {
	idx := strings.Index(updated, needle)
	if idx < 0 {
		return ""
	}
	startLine := strings.Count(updated[:idx], "\n") + 1
	endLine := startLine + strings.Count(needle, "\n")

	lines := strings.Split(updated, "\n")
	from := startLine - ctx
	if from < 1 {
		from = 1
	}
	to := endLine + ctx
	if to > len(lines) {
		to = len(lines)
	}

	var b strings.Builder
	for i := from; i <= to; i++ {
		line := lines[i-1]
		if len(line) > 200 {
			line = line[:200] + " [...]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i, line)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// indentMatch finds where old matches content line for line once leading
// whitespace (and trailing, and a CR) is set aside. With exactly one such
// place it returns content with those lines replaced by new, re-indented
// from old's indentation to the file's, and the re-indented new. It returns
// how many places matched and their first lines.
func indentMatch(content, old, new string) (string, string, int, []int) {
	oldLines := strings.Split(strings.TrimSuffix(old, "\n"), "\n")
	key := strings.TrimSpace
	solid := 0
	for _, l := range oldLines {
		if key(l) != "" {
			solid++
		}
	}
	if solid == 0 {
		return "", "", 0, nil
	}
	fileLines := strings.Split(content, "\n")
	var starts []int
	for i := 0; i+len(oldLines) <= len(fileLines); i++ {
		match := true
		for j, ol := range oldLines {
			if key(fileLines[i+j]) != key(ol) {
				match = false
				break
			}
		}
		if match {
			starts = append(starts, i)
		}
	}
	lines := make([]int, len(starts))
	for i, st := range starts {
		lines[i] = st + 1
	}
	if len(starts) != 1 {
		return "", "", len(starts), lines
	}
	at := starts[0]
	// How each indentation old used is spelled in the file.
	spell := map[string]string{}
	var keys []string
	cr := strings.HasSuffix(fileLines[at], "\r")
	for j, ol := range oldLines {
		if key(ol) == "" {
			continue
		}
		o, f := leadingSpace(ol), leadingSpace(fileLines[at+j])
		if _, seen := spell[o]; !seen {
			spell[o] = f
			keys = append(keys, o)
		}
	}
	newLines := strings.Split(strings.TrimSuffix(new, "\n"), "\n")
	for i, nl := range newLines {
		nl = strings.TrimSuffix(nl, "\r")
		if strings.TrimSpace(nl) != "" {
			w := leadingSpace(nl)
			best := ""
			for _, k := range keys {
				if strings.HasPrefix(w, k) && len(k) >= len(best) {
					best = k
				}
			}
			if f, ok := spell[best]; ok && (best != "" || spell[""] != "") {
				nl = f + w[len(best):] + nl[len(w):]
			}
		}
		if cr {
			nl += "\r"
		}
		newLines[i] = nl
	}
	reindented := strings.Join(newLines, "\n")
	out := append(append(append([]string{}, fileLines[:at]...), newLines...), fileLines[at+len(oldLines):]...)
	if strings.TrimSuffix(new, "\n") == "" {
		// A deletion takes its lines away rather than leaving one empty.
		out = append(append([]string{}, fileLines[:at]...), fileLines[at+len(oldLines):]...)
	}
	return strings.Join(out, "\n"), reindented, 1, lines
}

// leadingSpace is the run of spaces and tabs a line starts with.
func leadingSpace(l string) string {
	return l[:len(l)-len(strings.TrimLeft(l, " \t"))]
}

func intsToStrings(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = fmt.Sprint(n)
	}
	return out
}
