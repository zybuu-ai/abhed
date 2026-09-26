package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	defaultReadLimit = 2000
	maxLineLen       = 2000
)

// ---------------------------------------------------------------- read

type Read struct{}

func (Read) Name() string  { return "read" }
func (Read) Mutates() bool { return false }

func (Read) Description() string {
	return "Read a file from the filesystem. Returns numbered lines. Prefer this over `cat` via bash — output is bounded, numbered, and the read is tracked for edit safety. " +
		"Handles PDF, Word (.docx), PowerPoint (.pptx) and Excel (.xlsx) directly: " +
		"pass the path and the text is extracted for you. Do NOT use unzip, " +
		"pdftotext or similar via bash for these — read handles them."
}

func (Read) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"Absolute path to the file."},
    "offset":{"type":"integer","description":"1-indexed line to start from. Use with limit for large files."},
    "limit":{"type":"integer","description":"Max lines to return. Default 2000."}
  },
  "required":["path"]
}`)
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (r Read) Run(_ context.Context, s *Session, raw json.RawMessage) Result {
	var a readArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for read: %v", err)
	}
	path, err := s.Resolve(a.Path)
	if err != nil {
		return errf("%v", err)
	}

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return errf("File not found: %s. Use glob to locate it, or check that the parent directory exists.", a.Path)
	}
	if err != nil {
		return errf("Cannot read %s: %v", a.Path, err)
	}
	if info.IsDir() {
		return errf("%s is a directory, not a file. Use glob(%q) to list its contents.", a.Path, a.Path+"/**")
	}

	data, err := s.readFile(path)
	if err != nil {
		if os.IsPermission(err) {
			return errf("Permission denied reading %s.", a.Path)
		}
		return errf("Cannot read %s: %v", a.Path, err)
	}

	// Documents come before the binary check: a PDF or .docx IS binary on
	// disk, but it is a document a person expects the agent to read.
	var extracted DocumentKind
	if kind := DetectKind(data, path); kind != KindPlain {
		text, err := ExtractText(data, kind)
		if err != nil {
			return errf("Cannot read %s as a %s document: %v", a.Path, kind, err)
		}
		data = []byte(text)
		extracted = kind
	}

	if IsBinary(data) {
		return errf("%s appears to be a binary file (%d bytes). Abhed does not read binary content; use bash with an appropriate tool if you need to inspect it.", a.Path, len(data))
	}

	content := string(data)
	if extracted == KindPlain {
		// Track the full content, not the returned window: edit safety depends
		// on knowing what the model saw of the actual file.
		s.MarkRead(path, content)
	}
	// Extracted documents are deliberately NOT marked read. MarkRead exists so
	// edit can verify the file has not changed since the model saw it, and it
	// hashes what was read — for a PDF that is the extracted text, not the
	// bytes on disk. Recording it would let edit believe it had seen a file it
	// has never seen, and write over a binary document with plain text.

	if content == "" {
		if extracted != KindPlain {
			return ok("[%s contains no extractable text: %s]", extracted, a.Path)
		}
		return ok("[file exists but is empty: %s]", a.Path)
	}

	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	offset := a.Offset
	if offset < 1 {
		offset = 1
	}
	if offset > len(lines) {
		return errf("offset %d is past the end of %s (%d lines).", offset, a.Path, len(lines))
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}

	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	window := lines[offset-1 : end]

	var b strings.Builder
	for i, line := range window {
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "  [...line truncated]"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", offset+i, line)
	}

	truncated := end < len(lines)
	if truncated {
		fmt.Fprintf(&b, "\n[truncated: showing lines %d-%d of %d. Use offset/limit to read more.]",
			offset, end, len(lines))
	}
	return Result{Content: b.String(), Truncated: truncated}
}

// IsBinary reports whether data looks like something other than text: a NUL
// byte or invalid UTF-8 in the first 8000 bytes.
func IsBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
		// The cut can land inside a multi-byte character, which is not
		// evidence of binary: step back to where that character starts.
		for i := 0; i < utf8.UTFMax && n > 0 && !utf8.RuneStart(data[n]); i++ {
			n--
		}
	}
	head := data[:n]
	if !utf8.Valid(head) {
		return true
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- write

type Write struct{}

func (Write) Name() string  { return "write" }
func (Write) Mutates() bool { return true }

func (Write) Description() string {
	return "Write content to a file, creating it or overwriting it entirely. For modifying part of an existing file, use `edit` instead — it is safer and cheaper."
}

func (Write) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"Absolute path."},
    "content":{"type":"string","description":"Complete file content."}
  },
  "required":["path","content"]
}`)
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Precheck refuses a path the write could never use, before anyone is asked.
func (Write) Precheck(s *Session, raw json.RawMessage) error { return precheckPath(s, raw) }

func (Write) Run(ctx context.Context, s *Session, raw json.RawMessage) Result {
	var a writeArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for write: %v", err)
	}
	path, err := s.Resolve(a.Path)
	if err != nil {
		return errf("%v", err)
	}

	existed := false
	if _, err := os.Stat(path); err == nil {
		existed = true
		// Refuse to destroy content the model has not seen.
		if !s.WasRead(path) {
			return errf("Refusing to overwrite %s — it exists but has not been read this session. Call read(%q) first to see what you would replace.", a.Path, a.Path)
		}
	}

	if dir := filepath.Dir(path); dir != "" {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return errf("Parent directory does not exist: %s. Create it with bash `mkdir -p` first.", dir)
		}
	}

	mode := os.FileMode(0o644)
	var before []byte
	if existed {
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if s.Syntax != SyntaxOff {
			if before, err = s.readFile(path); err != nil {
				return errf("Cannot read %s: %v", a.Path, err)
			}
		}
	}
	note, refuse := s.syntaxVerdict(ctx, path, before, existed, []byte(a.Content))
	if refuse {
		return errf("%s", note)
	}

	s.recordChange(path)
	if err := s.atomicWrite(path, []byte(a.Content), mode); err != nil {
		return errf("Write failed for %s: %v", a.Path, err)
	}
	s.MarkRead(path, a.Content)

	verb := "Created"
	if existed {
		verb = "Overwrote"
	}
	lines := strings.Count(a.Content, "\n")
	if a.Content != "" && !strings.HasSuffix(a.Content, "\n") {
		lines++
	}
	return ok("%s %s (%d bytes, %d lines).%s", verb, s.Rel(path), len(a.Content), lines, suffix(note))
}

// atomicWrite writes via a temp file in the same directory then renames, so a
// crash mid-write leaves the original intact rather than a truncated file.
// It works inside the root holding the path; see Confined.WriteAtomic.
func (s *Session) atomicWrite(path string, data []byte, mode os.FileMode) error {
	c, at, err := s.confine(path)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.WriteAtomic(at, data, mode)
}

// readFile reads a file the session resolved, inside the root holding it;
// see Confined.OpenRead.
func (s *Session) readFile(path string) ([]byte, error) {
	c, at, err := s.confine(path)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ReadFile(at)
}

// ReadFile reads a file of the session's roots as the file tools do: inside
// the root that holds it, never through a link that leads out or into
// Abhed's state. For callers outside the tools, such as /diff.
func (s *Session) ReadFile(path string) ([]byte, error) { return s.readFile(path) }

// RestoreFile writes a file's earlier content back, as the file tools write,
// for /undo.
func (s *Session) RestoreFile(path string, data []byte) error {
	return s.atomicWrite(path, data, 0o600)
}

// RemoveFile removes a file inside the root that holds it, for /undo of a
// creation; a link swapped in cannot send the removal elsewhere.
func (s *Session) RemoveFile(path string) error {
	c, at, err := s.confine(path)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Remove(at)
}

// confine opens the session root that holds path, by either spelling, and
// returns the path to use in it. A path under one root that a link leads into
// another, such as an added directory, is used as it resolves.
func (s *Session) confine(path string) (*Confined, string, error) {
	s.mu.Lock()
	type pair struct{ dir, alt string }
	pairs := []pair{{s.Root, s.rawRoot}}
	for i, r := range s.Roots {
		alt := ""
		if i < len(s.rawRoots) {
			alt = s.rawRoots[i]
		}
		pairs = append(pairs, pair{r, alt})
	}
	s.mu.Unlock()
	set := s.stateSet()
	real := RealPath(path)
	for _, want := range []string{path, real} {
		for _, p := range pairs {
			c, err := set.Confine(p.dir, p.alt)
			if err != nil {
				continue
			}
			// The path must be where it resolves, too: a lexical match whose
			// link leads into another root is placed in that root instead.
			if c.Contains(want) && c.Contains(real) {
				return c, want, nil
			}
			if want == real && c.Contains(real) {
				return c, real, nil
			}
			c.Close()
		}
	}
	return nil, "", ErrOutside
}
