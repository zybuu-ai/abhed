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
	"sync"
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

// pythonStrict is the oldest interpreter trusted to refuse a change: an older
// one would reject syntax the project's own Python accepts, so it only warns.
var pythonStrict = [2]int{3, 12}

// verdict is what a parser said about a file.
type verdict struct {
	checked bool   // a parser for this language was available and answered
	err     error  // nil when the content parses
	strict  bool   // the parser may refuse a change, not only warn
	by      string // which parser, for the message: "python3.9", "go"
}

// parses reports whether content parses in the language its path names.
func (s *Session) parses(ctx context.Context, path string, content []byte) verdict {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		_, err := parser.ParseFile(token.NewFileSet(), filepath.Base(path), content, parser.SkipObjectResolution)
		return verdict{checked: true, err: err, strict: true, by: "go"}
	case ".json":
		var v any
		if err := json.Unmarshal(content, &v); err != nil {
			return verdict{checked: true, err: fmt.Errorf("invalid JSON: %w", err), strict: true, by: "json"}
		}
		return verdict{checked: true, strict: true, by: "json"}
	case ".py":
		return s.parsesPython(ctx, path, content)
	}
	return verdict{}
}

// interpreter is the python3 the check runs: the real file behind the one on
// PATH, never one inside the session's roots, which the agent can write.
func (s *Session) interpreter() (string, [2]int, bool) {
	found, err := exec.LookPath("python3")
	if err != nil {
		return "", [2]int{}, false
	}
	real, err := filepath.EvalSymlinks(found)
	if err != nil {
		return "", [2]int{}, false
	}
	for _, root := range append([]string{s.Root}, s.Roots...) {
		if rel, err := filepath.Rel(root, real); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return "", [2]int{}, false
		}
	}
	ver, ok := pythonVersion(real)
	return real, ver, ok
}

var pyVersions sync.Map // interpreter path -> [2]int, successes only

// pythonVersion asks the interpreter its version, under the same isolation
// and bound as the check itself. A failure is not cached: the next edit asks again.
func pythonVersion(py string) ([2]int, bool) {
	if v, ok := pyVersions.Load(py); ok {
		return v.([2]int), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), pythonTimeout)
	defer cancel()
	cmd := pythonCommand(ctx, py, "import sys; print(*sys.version_info[:2])")
	out, err := cmd.Output()
	var v [2]int
	if err != nil {
		return v, false
	}
	if n, _ := fmt.Sscan(string(out), &v[0], &v[1]); n != 2 || v[0] == 0 {
		return v, false
	}
	pyVersions.Store(py, v)
	return v, true
}

// pythonCommand runs py isolated: -I ignores the environment, user site and
// current directory, -S skips site and so .pth files; no env, temp dir cwd.
func pythonCommand(ctx context.Context, py, script string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, py, append([]string{"-I", "-S", "-c", script}, args...)...)
	cmd.Dir = os.TempDir()
	cmd.Env = []string{}
	cmd.WaitDelay = time.Second // a child holding the pipe cannot outlast the timeout
	return cmd
}

// parsesPython compiles the source without running it; the real interpreter
// file skips any virtualenv. See pythonCommand for the isolation.
func (s *Session) parsesPython(ctx context.Context, path string, content []byte) verdict {
	py, ver, ok := s.interpreter()
	if !ok {
		return verdict{}
	}
	ctx, cancel := context.WithTimeout(ctx, pythonTimeout)
	defer cancel()
	cmd := pythonCommand(ctx, py, "import sys; compile(sys.stdin.buffer.read(), sys.argv[1], 'exec')", filepath.Base(path))
	cmd.Stdin = bytes.NewReader(content)
	stderr := &capped{max: 16 << 10}
	cmd.Stderr = stderr
	by := fmt.Sprintf("python%d.%d", ver[0], ver[1])
	strict := ver[0] > pythonStrict[0] || (ver[0] == pythonStrict[0] && ver[1] >= pythonStrict[1])
	if err := cmd.Run(); err != nil {
		// Only a compile error is a verdict; a timeout or a crashing
		// interpreter says nothing about the file.
		if ctx.Err() != nil || !pythonSyntaxError.MatchString(stderr.String()) {
			return verdict{}
		}
		return verdict{checked: true, err: fmt.Errorf("%s", pythonError(stderr.String())), strict: strict, by: by}
	}
	return verdict{checked: true, strict: strict, by: by}
}

// capped keeps the first max bytes written to it.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
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

// notCode are files where lines starting with + or - are ordinary content.
var notCode = map[string]bool{".md": true, ".markdown": true, ".rst": true, ".txt": true, ".csv": true,
	".yaml": true, ".yml": true, ".diff": true, ".patch": true}

// looksLikeDiff is true when the new text is a pasted hunk: every non-empty
// line is marked and it both removes and adds. Negations and lists do not.
func looksLikeDiff(path, oldText, newText string) bool {
	if notCode[strings.ToLower(filepath.Ext(path))] {
		return false
	}
	for _, l := range strings.Split(oldText, "\n") {
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			return false
		}
	}
	var plus, minus bool
	for _, l := range strings.Split(newText, "\n") {
		switch {
		case strings.TrimSpace(l) == "":
		case strings.HasPrefix(l, "+"):
			plus = true
		case strings.HasPrefix(l, "-"):
			minus = true
		default:
			return false
		}
	}
	return plus && minus
}

// parse is the parser syntaxVerdict uses; tests replace it.
var parse = (*Session).parses

// syntaxVerdict decides what a change that breaks parsing gets. A file broken
// before, or a new one, is never refused: a refactor must not stall half-way.
func (s *Session) syntaxVerdict(ctx context.Context, path string, before []byte, existed bool, after []byte) (note string, refuse bool) {
	if s.Syntax == SyntaxOff {
		return "", false
	}
	a := parse(s, ctx, path, after)
	if !a.checked || a.err == nil {
		return "", false
	}
	rel := s.Rel(path)
	if !existed {
		return fmt.Sprintf("Warning: %s does not parse (%s) — %v", rel, a.by, a.err), false
	}
	b := parse(s, ctx, path, before)
	switch {
	case b.checked && b.err != nil:
		return fmt.Sprintf("Note: %s still does not parse (%s) — %v", rel, a.by, a.err), false
	case !b.checked:
		return fmt.Sprintf("Warning: %s does not parse (%s) — %v", rel, a.by, a.err), false
	case s.Syntax == SyntaxRefuse && a.strict:
		return fmt.Sprintf("%s %s would no longer parse (%s) — %s The file is unchanged; fix the new text and try again.",
			NotApplied, rel, a.by, sentence(a.err.Error())), true
	}
	return fmt.Sprintf("Warning: %s no longer parses (%s) — %v", rel, a.by, a.err), false
}

// suffix puts a warning on its own line after a tool's message.
func suffix(note string) string {
	if note == "" {
		return ""
	}
	return "\n" + note
}

// sentence ends a parser message with one stop, whatever it ended with.
func sentence(msg string) string {
	msg = strings.TrimSpace(msg)
	if strings.HasSuffix(msg, ".") || strings.HasSuffix(msg, "?") || strings.HasSuffix(msg, "!") {
		return msg
	}
	return msg + "."
}
