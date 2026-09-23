package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SyntaxMode is what an edit or write does when it would leave a file that
// parsed before no longer parsing.
type SyntaxMode int

const (
	// SyntaxRefuse leaves the file unchanged and tells the model why. It is
	// the zero value, so every session checks unless told otherwise.
	SyntaxRefuse SyntaxMode = iota
	// SyntaxReport applies the change and warns: for a person saving work
	// in progress, who must not be blocked.
	SyntaxReport
	// SyntaxOff skips the check.
	SyntaxOff
)

// ParseSyntaxMode reads the `tools.syntax_check` setting.
func ParseSyntaxMode(s string) (SyntaxMode, error) {
	switch s {
	case "", "refuse":
		return SyntaxRefuse, nil
	case "report":
		return SyntaxReport, nil
	case "off":
		return SyntaxOff, nil
	}
	return SyntaxRefuse, fmt.Errorf("unknown tools.syntax_check %q (want refuse, report or off)", s)
}

// NotApplied opens every refusal the check makes, so the record and HawkEYE
// can tell a refused change from any other failed call.
const NotApplied = "Not applied:"

// pythonTimeout bounds the one check that starts a process.
const pythonTimeout = 5 * time.Second

// parses reports whether content parses in the language its path names.
// checked is false when there is no parser for that language here.
func parses(ctx context.Context, path string, content []byte) (checked bool, err error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		_, err := parser.ParseFile(token.NewFileSet(), filepath.Base(path), content, parser.SkipObjectResolution)
		return true, err
	case ".json":
		var v any
		if err := json.Unmarshal(content, &v); err != nil {
			return true, fmt.Errorf("invalid JSON: %w", err)
		}
		return true, nil
	case ".py":
		return parsesPython(ctx, path, content)
	}
	return false, nil
}

// parsesPython compiles the source without running it. -I keeps the
// interpreter from reading the workspace, the environment or user site
// packages, so nothing in the checked tree can execute.
func parsesPython(ctx context.Context, path string, content []byte) (bool, error) {
	py, err := exec.LookPath("python3")
	if err != nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, pythonTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-I", "-c",
		"import sys; compile(sys.stdin.buffer.read(), sys.argv[1], 'exec')", filepath.Base(path))
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Only a compile error is a verdict; a timeout or a crashing
		// interpreter says nothing about the file.
		if ctx.Err() != nil || !pythonSyntaxError.MatchString(stderr.String()) {
			return false, nil
		}
		return true, fmt.Errorf("%s", pythonError(stderr.String()))
	}
	return true, nil
}

// pythonSyntaxError matches the last line of a compile traceback:
// SyntaxError and its subclasses IndentationError and TabError.
var pythonSyntaxError = regexp.MustCompile(`(?m)^(SyntaxError|IndentationError|TabError): `)

// pythonError keeps the line number and the message from a compile traceback.
func pythonError(tb string) string {
	var where, what string
	for _, l := range strings.Split(strings.TrimSpace(tb), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "File ") {
			if i := strings.Index(t, "line "); i >= 0 {
				where = t[i:]
			}
		}
		if t != "" {
			what = t
		}
	}
	if where != "" {
		return where + ": " + what
	}
	return what
}

// looksLikeDiff is true when every non-empty line of the new text carries a
// diff marker the old text does not: a model pasting a patch, not code.
func looksLikeDiff(oldText, newText string) bool {
	marked := func(l string) bool { return strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") }
	for _, l := range strings.Split(oldText, "\n") {
		if marked(l) {
			return false
		}
	}
	lines := 0
	for _, l := range strings.Split(newText, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !marked(l) {
			return false
		}
		lines++
	}
	return lines > 0
}

// syntaxVerdict decides what happens to a change that leaves a file not
// parsing. A file that did not parse before is left to the model: blocking
// it would stop a refactor half-way. A new file is never refused.
func (s *Session) syntaxVerdict(ctx context.Context, path string, before []byte, existed bool, after []byte) (note string, refuse bool) {
	if s.Syntax == SyntaxOff {
		return "", false
	}
	checked, errAfter := parses(ctx, path, after)
	if !checked || errAfter == nil {
		return "", false
	}
	rel := s.Rel(path)
	if !existed {
		return fmt.Sprintf("Warning: %s does not parse — %v", rel, errAfter), false
	}
	if _, errBefore := parses(ctx, path, before); errBefore != nil {
		return "", false
	}
	if s.Syntax == SyntaxRefuse {
		return fmt.Sprintf("%s %s would no longer parse — %v. The file is unchanged; fix the new text and try again.",
			NotApplied, rel, errAfter), true
	}
	return fmt.Sprintf("Warning: %s no longer parses — %v", rel, errAfter), false
}

// suffix puts a warning on its own line after a tool's message.
func suffix(note string) string {
	if note == "" {
		return ""
	}
	return "\n" + note
}
