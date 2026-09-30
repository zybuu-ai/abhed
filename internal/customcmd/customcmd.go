// Package customcmd loads custom slash commands: markdown files whose body
// is a prompt, with an optional header naming a description, an argument
// hint, the tools the command's turn may use and the model it runs on.
//
// Commands come from three places. The organisation's managed directory and
// the person's own directories always load. A workspace's .abhed/commands came
// with the repository, and a command is instructions to the agent, so those
// files load only under a trust decision bound to their exact content, as
// agent definitions do: a change to any of them needs trust again.
//
// A command can only narrow. Its tool list is a subset of the session's tools,
// never an allow rule; it cannot change the permission mode, add rules, or
// name a model that is not configured.
package customcmd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/frontmatter"
	"github.com/zybuu-ai/abhed/internal/nlink"
)

// Sources a command can come from.
const (
	SourceManaged   = "managed"
	SourceUser      = "user"
	SourceWorkspace = "workspace"
)

// WorkspaceDir is where a workspace keeps its commands.
const WorkspaceDir = ".abhed/commands"

// Bounds on what one directory holds.
const (
	MaxFiles     = 128
	MaxFileBytes = 64 << 10
	MaxDesc      = 300
)

// ManagedDir is the organisation's directory; a variable for tests.
var ManagedDir = filepath.Join("/etc", "abhed", "commands")

// Command is one custom command.
type Command struct {
	Name         string // "/review", or "/frontend:test" for frontend/test.md
	Description  string
	ArgumentHint string
	// AllowedTools narrows the tools the command's turn may use; none
	// leaves the session's tools as they are.
	AllowedTools []string
	// Model is a configured provider the turn runs on; "" keeps the current.
	Model  string
	Body   string
	Path   string
	Source string
	SHA256 string // of the file's content
}

// File is one command file as read and hashed.
type File struct {
	Rel  string // relative to its directory, slash-separated
	Path string
	Data []byte
	// Key is what the trust hash names the file by, when not Rel: its path
	// in the workspace.
	Key string
}

// nameRE is a command name segment: plain ASCII, as the registry requires.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// toolRE is one tool name, or the mcp__<server>__* wildcard.
var toolRE = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9_.-]{0,127}|mcp__[A-Za-z0-9_.-]+__\*)$`)

// honoured are the header keys a command may set.
var honoured = map[string]bool{"description": true, "argumenthint": true, "allowedtools": true, "model": true}

func keyOf(k string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.Trim(strings.TrimSpace(k), `"'`)))
}

// ReadDir reads a directory's command files, and those one level down (a
// namespace), sorted. A file that is a link, has a second name or is too
// large is refused. A missing directory is empty.
func ReadDir(dir string) ([]File, []error) {
	var out []File
	var errs []error
	var walk func(d, prefix string, depth int)
	walk = func(d, prefix string, depth int) {
		entries, err := os.ReadDir(d)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("custom commands: %w", err))
			return
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			p := filepath.Join(d, name)
			if e.IsDir() {
				if depth == 0 {
					walk(p, prefix+name+"/", depth+1)
				}
				continue
			}
			if !strings.HasSuffix(name, ".md") {
				continue
			}
			if len(out) == MaxFiles {
				errs = append(errs, fmt.Errorf("more than %d custom commands in %s; the rest are not read", MaxFiles, dir))
				return
			}
			data, err := readFile(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("custom command %s refused: %w", p, err))
				continue
			}
			out = append(out, File{Rel: prefix + name, Path: p, Data: data})
		}
	}
	walk(dir, "", 0)
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, errs
}

// readFile reads a command file: a regular file with one name.
func readFile(p string) ([]byte, error) {
	return ReadRegular(p, MaxFileBytes)
}

// openRegular opens a checked file; a variable so a test can swap the file
// between the check and the open.
var openRegular = openChecked

// openChecked opens without following a last link and without blocking.
func openChecked(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|noFollow, 0) // #nosec G304 -- a file the person or operator named, checked first
}

// ReadRegular reads a regular file with one name and at most max bytes. The
// last component is opened without following a link, and the open file must
// be the one checked, so a swap between the check and the read is refused.
func ReadRegular(p string, max int64) ([]byte, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file; a link is not followed")
	}
	f, err := openRegular(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("changed while it was read")
	}
	if n := nlink.Of(opened); n > 1 {
		return nil, fmt.Errorf("has %d names; a file with a second name is not read", n)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("larger than %d KiB", max>>10)
	}
	return data, nil
}

// ReadWorkspace reads a workspace's commands and the hash its trust covers:
// one hash over every file's path and content. The directory and .abhed must
// be directories of their own, not links.
func ReadWorkspace(workspace string) ([]File, string, []error) {
	dir := filepath.Join(workspace, filepath.FromSlash(WorkspaceDir))
	for _, d := range []string{filepath.Join(workspace, ".abhed"), dir} {
		info, err := os.Lstat(d)
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", []error{err}
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, "", []error{fmt.Errorf("%s is not a directory of its own; no command is read from it", d)}
		}
	}
	files, errs := ReadDir(dir)
	for i := range files {
		files[i].Key = WorkspaceDir + "/" + files[i].Rel
	}
	return files, HashFiles(files), errs
}

// ReadWorkspaceDirs reads commands from directories inside the workspace
// that a configuration named, with their paths in it as the trust hash's
// keys: they came with the repository as much as .abhed/commands does.
func ReadWorkspaceDirs(workspace string, dirs []string) ([]File, []error) {
	var out []File
	var errs []error
	for _, d := range dirs {
		files, e := ReadDir(d)
		errs = append(errs, e...)
		for _, f := range files {
			if rel, err := filepath.Rel(workspace, f.Path); err == nil {
				f.Key = filepath.ToSlash(rel)
			}
			out = append(out, f)
		}
	}
	return out, errs
}

// Inside reports whether dir lies in the workspace: dir, or a folder above
// it, is the workspace's own folder by identity, with links resolved. An
// identity check holds on a disk that folds case, where two spellings name
// one folder. A dir that does not exist yet is judged by its nearest parent.
func Inside(workspace, dir string) bool {
	root, err := os.Stat(workspace)
	if err != nil {
		return false
	}
	for _, p := range []string{filepath.Clean(dir), resolved(dir)} {
		for ; ; p = filepath.Dir(p) {
			if info, err := os.Stat(p); err == nil && os.SameFile(root, info) {
				return true
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return false
}

// resolved is p with its links followed, as far as the path exists.
func resolved(p string) string {
	p = filepath.Clean(p)
	var rest []string
	for q := p; ; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		if filepath.Dir(q) == q {
			return p
		}
		rest = append([]string{filepath.Base(q)}, rest...)
	}
}

// HashFiles is the content hash a trust decision covers.
func HashFiles(files []File) string {
	h := sha256.New()
	for _, f := range files {
		sum := sha256.Sum256(f.Data)
		key := f.Key
		if key == "" {
			key = f.Rel
		}
		fmt.Fprintf(h, "%s\x00%s\n", key, hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Parse reads one command file. models are the configured provider names
// the model key may choose.
func Parse(f File, source string, models []string) (*Command, error) {
	rel := strings.TrimSuffix(f.Rel, ".md")
	parts := strings.Split(rel, "/")
	for _, p := range parts {
		if !nameRE.MatchString(p) {
			return nil, fmt.Errorf("%q is not a plain command name (letters, digits, - _ .)", p)
		}
	}
	sum := sha256.Sum256(f.Data)
	c := &Command{Name: "/" + strings.Join(parts, ":"), Path: f.Path, Source: source, SHA256: hex.EncodeToString(sum[:])}
	text := string(f.Data)
	if !strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "---") {
		c.Body = strings.TrimSpace(text)
		return c, checkBody(c)
	}
	doc, err := frontmatter.Parse(text)
	if err != nil {
		return nil, err
	}
	c.Body = doc.Body
	for _, fld := range doc.Top() {
		k := keyOf(fld.Key)
		if !honoured[k] {
			return nil, fmt.Errorf("key %q is not one a command may set (description, argument-hint, allowed-tools, model); "+
				"a command cannot change the mode, rules or sandbox", fld.Key)
		}
		if fld.Kind == frontmatter.Map {
			return nil, fmt.Errorf("key %q has a nested value", fld.Key)
		}
		switch k {
		case "description":
			c.Description = oneLine(fld.Value)
			if len(c.Description) > MaxDesc {
				return nil, fmt.Errorf("description longer than %d characters", MaxDesc)
			}
		case "argumenthint":
			c.ArgumentHint = oneLine(fld.Value)
		case "model":
			c.Model = strings.TrimSpace(fld.Value)
			if c.Model != "" && !contains(models, c.Model) {
				return nil, fmt.Errorf("model %q is not a configured provider", c.Model)
			}
		case "allowedtools":
			list := fld.List
			if fld.Kind != frontmatter.List {
				list = strings.FieldsFunc(fld.Value, func(r rune) bool { return r == ',' || r == ' ' })
			}
			for _, t := range list {
				t = strings.TrimSpace(t)
				if !toolRE.MatchString(t) {
					return nil, fmt.Errorf("allowed-tools names tools only; %q is not a tool name (rules belong to the permissions)", t)
				}
				c.AllowedTools = append(c.AllowedTools, t)
			}
			if len(c.AllowedTools) == 0 {
				return nil, errors.New("allowed-tools is empty; leave it out to keep the session's tools")
			}
		}
	}
	return c, checkBody(c)
}

func checkBody(c *Command) error {
	if strings.TrimSpace(c.Body) == "" {
		return errors.New("the command has no body")
	}
	return nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// argRE is $ARGUMENTS or a positional $1 to $9.
var argRE = regexp.MustCompile(`\$(ARGUMENTS|[1-9])`)

// Expand substitutes the arguments into a body: $ARGUMENTS is all of them,
// $1 to $9 each, split as a shell would split plain words, with "quoted"
// words kept together. A body that names no argument gets them appended.
func Expand(body, args string) string {
	words := splitArgs(args)
	used := false
	out := argRE.ReplaceAllStringFunc(body, func(m string) string {
		used = true
		if m == "$ARGUMENTS" {
			return args
		}
		n, _ := strconv.Atoi(m[1:])
		if n <= len(words) {
			return words[n-1]
		}
		return ""
	})
	if !used && strings.TrimSpace(args) != "" {
		out += "\n\nArguments: " + args
	}
	return out
}

func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote = r
		case quote == 0 && (r == ' ' || r == '\t'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// inlineRE is an inline shell line in a body: !`command`.
var inlineRE = regexp.MustCompile("!`([^`\n]+)`")

// InlineCommands are the shell lines a body runs, in order.
func InlineCommands(body string) []string {
	var out []string
	for _, m := range inlineRE.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// ReplaceInline replaces each inline shell line with what out returns for it.
func ReplaceInline(body string, out func(cmd string) string) string {
	return inlineRE.ReplaceAllStringFunc(body, func(m string) string {
		return out(inlineRE.FindStringSubmatch(m)[1])
	})
}

// Options say where commands come from.
type Options struct {
	// ManagedDir is the organisation's directory; "" skips it.
	ManagedDir string
	// UserDirs are the person's directories, earliest first; a later one
	// wins a name.
	UserDirs []string
	// Workspace are the workspace's files when they are trusted; leave it
	// nil when not.
	Workspace []File
	// Models are the provider names a command may choose.
	Models []string
}

// Load reads every command. The organisation's names come first and are
// never replaced; then the person's, then the workspace's. A file that does
// not load is reported and skipped.
func Load(o Options) ([]*Command, []error) {
	var errs []error
	byName := map[string]*Command{}
	var out []*Command
	add := func(files []File, source string) {
		for _, f := range files {
			c, err := Parse(f, source, o.Models)
			if err != nil {
				errs = append(errs, fmt.Errorf("custom command %s refused: %w", f.Path, err))
				continue
			}
			if prev, taken := byName[c.Name]; taken {
				errs = append(errs, fmt.Errorf("custom command %s is shadowed by %s (%s)", f.Path, prev.Path, prev.Source))
				continue
			}
			byName[c.Name] = c
			out = append(out, c)
		}
	}
	if o.ManagedDir != "" {
		files, e := ReadDir(o.ManagedDir)
		errs = append(errs, e...)
		add(files, SourceManaged)
	}
	for i := len(o.UserDirs) - 1; i >= 0; i-- {
		files, e := ReadDir(o.UserDirs[i])
		errs = append(errs, e...)
		add(files, SourceUser)
	}
	// Last: a repository cannot take a name the person or the organisation uses.
	add(o.Workspace, SourceWorkspace)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}
