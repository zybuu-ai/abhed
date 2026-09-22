package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Session carries the per-session state tools need: the workspace root that
// scopes all filesystem access, and the read-tracking that makes editing safe.
type Session struct {
	Root string // absolute workspace root
	Cwd  string // persists across bash calls (shell state does not)

	// rawRoot and rawRoots hold the roots before symlink resolution, used
	// only by the lexical traversal check. See lexicalRoots.
	rawRoot  string
	rawRoots []string

	// Roots are additional directories the agent may reach, beyond Root.
	//
	// The scoping boundary exists so that a prompt-injected agent cannot read
	// your SSH keys or write outside the work at hand, and it must not be
	// removable by asking — a model that can talk its way out of the sandbox
	// has no sandbox. But a single root is too rigid for real work: an agent
	// asked to port a change between two checkouts, or to read a shared
	// library alongside the service using it, genuinely needs both.
	//
	// So the boundary stays absolute and the OPERATOR moves it: extra roots
	// come from config or the command line, never from the model. Each is
	// symlink-resolved at construction for the same reason Root is.
	Roots []string

	// Checkpoint records a file's content immediately before the agent changes
	// it, backing /undo. Set by the caller; nil disables checkpointing.
	//
	// It lives on the Session rather than in each tool so that every mutating
	// tool gets it by construction — a new tool cannot forget to call it.
	Checkpoint func(path string, before []byte, existed bool)

	mu    sync.Mutex
	reads map[string]string // abs path -> content hash at time of read
}

// snapshot captures a file's current content before it is modified. Called by
// the mutating tools through recordChange.
func (s *Session) recordChange(path string) {
	if s.Checkpoint == nil {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// A missing file is a valid checkpoint: undo means "delete it again".
		s.Checkpoint(path, nil, false)
		return
	}
	s.Checkpoint(path, data, true)
}

func NewSession(root string) (*Session, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	// Resolve symlinks so a symlinked root cannot be used to escape scoping.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	raw, _ := filepath.Abs(root)
	return &Session{Root: abs, Cwd: abs, rawRoot: filepath.Clean(raw),
		reads: make(map[string]string)}, nil
}

// Fork returns a session over the same roots with its own working directory
// and read tracking. The workbench gives one to the person at the keyboard, so
// a cd in their terminal never moves the agent, and changes still checkpoint.
func (s *Session) Fork() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &Session{
		Root: s.Root, Cwd: s.Root, rawRoot: s.rawRoot,
		rawRoots:   append([]string(nil), s.rawRoots...),
		Roots:      append([]string(nil), s.Roots...),
		Checkpoint: s.Checkpoint,
		reads:      make(map[string]string),
	}
}

// AddRoot grants access to another directory. Called from config or a CLI
// flag at startup; there is deliberately no tool that reaches this.
func (s *Session) AddRoot(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	} else {
		return fmt.Errorf("%s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// Refuse roots that would defeat the boundary entirely. Granting "/" or a
	// home directory is almost never what someone means, and it silently puts
	// credentials, SSH keys and browser profiles in reach of any injected
	// instruction in a file the agent reads.
	if abs == "/" {
		return fmt.Errorf("refusing to add / as a workspace root: " +
			"that removes the boundary entirely. Add the specific project directory")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if resolved, err := filepath.EvalSymlinks(home); err == nil {
			home = resolved
		}
		if abs == home {
			return fmt.Errorf("refusing to add your home directory as a workspace "+
				"root: it puts ~/.ssh, ~/.aws and browser profiles in reach of "+
				"anything the agent reads. Add the project directory instead (%s/...)", home)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.Roots {
		if existing == abs {
			return nil
		}
	}
	s.Roots = append(s.Roots, abs)
	if raw, err := filepath.Abs(dir); err == nil {
		s.rawRoots = append(s.rawRoots, filepath.Clean(raw))
	}
	return nil
}

// allowedRoots returns every directory this session may reach.
func (s *Session) allowedRoots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.Roots)+1)
	out = append(out, s.Root)
	return append(out, s.Roots...)
}

// within reports whether an absolute, cleaned path sits inside any root.
func within(path string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Resolve validates a model-supplied path and returns its absolute form.
//
// Two guarantees: the path is absolute (relative paths are ambiguous across
// turns after a cd), and it stays inside the workspace. Both failures return
// messages that tell the model how to correct the call.
func (s *Session) Resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute. Did you mean %s?",
			filepath.Join(s.Cwd, path))
	}

	clean := filepath.Clean(path)
	if isHarnessState(clean) {
		return "", fmt.Errorf("%s is Abhed's own state (%s holds its policy, users and keys). "+
			"The agent cannot read or change it in any mode; the operator edits it by hand. Do not retry",
			path, StateDir)
	}
	roots := s.allowedRoots()

	// Compare against the symlink-resolved roots. We resolve the deepest
	// existing ancestor so that a path to a not-yet-created file still gets
	// checked against its real parent directory.
	check := clean
	for {
		if resolved, err := filepath.EvalSymlinks(check); err == nil {
			if !within(resolved, roots) {
				return "", s.denied(path, roots)
			}
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break // reached the filesystem root without resolving
		}
		check = parent
	}

	// Also check the lexical path, to catch traversal on paths that do not
	// exist. Compare against the UNRESOLVED roots as well: on macOS /var is a
	// symlink to /private/var, so a caller naming a path under /var would fail
	// this check against the resolved root even though the resolved comparison
	// above already accepted it.
	if !within(clean, roots) && !within(clean, s.lexicalRoots()) {
		return "", s.denied(path, roots)
	}
	return clean, nil
}

// StateDir is the directory, in the workspace and in the home directory, that
// holds Abhed's own configuration, users and keys.
const StateDir = ".abhed"

// isHarnessState reports whether a cleaned path has StateDir as a component.
//
// It is a boundary and not a rule: a rule can be edited away by whoever can
// write the configuration, and this is what stops the agent being that
// whoever. A prompt-injected agent that could rewrite its own deny list, or
// add a user for the next start, would have no boundary at all.
func isHarnessState(clean string) bool {
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == StateDir {
			return true
		}
	}
	return false
}

// lexicalRoots returns the roots as given, before symlink resolution, so the
// lexical traversal check does not reject a path that names a symlinked
// ancestor (/var vs /private/var on macOS).
func (s *Session) lexicalRoots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.rawRoots)+1)
	if s.rawRoot != "" {
		out = append(out, s.rawRoot)
	}
	return append(out, s.rawRoots...)
}

// denied explains the refusal and, importantly, how to lift it. A bare
// "access denied" sent the model into rephrasing the same call: it cannot tell
// a permanent boundary from a transient error, so it retries. Naming the
// operator action ends that loop, and tells the person reading the transcript
// what to actually do.
func (s *Session) denied(path string, roots []string) error {
	return fmt.Errorf("%s is outside this session's workspace. Reachable: %s. "+
		"Do not retry; ask the user to restart Abhed in that directory "+
		"(abhed -C <dir>) or grant it with --add-dir <dir>",
		path, strings.Join(roots, ", "))
}

// MarkRead records that a file was read, with a hash of what was seen.
//
// This is the single most effective guard against destructive edits: write and
// edit both require a prior read, so the model can never replace content it has
// not observed (docs §06 §3-4).
func (s *Session) MarkRead(path, content string) {
	sum := sha256.Sum256([]byte(content))
	s.mu.Lock()
	s.reads[path] = hex.EncodeToString(sum[:])
	s.mu.Unlock()
}

func (s *Session) WasRead(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, found := s.reads[path]
	return found
}

// ChangedSinceRead reports whether the file on disk differs from what was read.
// Catches the case where an external process (or a bash command) modified a
// file between the model reading it and editing it.
func (s *Session) ChangedSinceRead(path string) bool {
	s.mu.Lock()
	want, found := s.reads[path]
	s.mu.Unlock()
	if !found {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) != want
}

// Rel renders a path relative to the workspace root for display. Output stays
// short and clickable without leaking absolute paths into the transcript.
func (s *Session) Rel(path string) string {
	if rel, err := filepath.Rel(s.Root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}
