// Package agentdefs loads subagent definitions: markdown files whose
// frontmatter names a role (name, description, tools, model and a few limits)
// and whose body is the role's instructions.
//
// Definitions load from three places, highest first: the organisation's
// managed directory, the workspace's .abhed/agents when the person trusted
// that exact content, and the operator's agents.dirs. Every key may only
// narrow what the spawning session could do. A key that would concern
// authority and that Abhed does not honour refuses the whole definition, so an
// author who expected a restriction never gets a looser agent in silence.
package agentdefs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/frontmatter"
)

// Limits on one definition.
const (
	MaxDescription = 300
	MaxBody        = 16 << 10
	MaxTurns       = 100
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// toolRE is one tool name, or the mcp__<server>__* wildcard.
var toolRE = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9_.-]{0,127}|mcp__[A-Za-z0-9_.-]+__\*)$`)

// Options say where to look and what the definitions may name.
type Options struct {
	// ManagedDir is the organisation's directory; empty skips it.
	ManagedDir string
	// Workspace are the trusted workspace files, as hashed. Leave it empty
	// for an untrusted workspace.
	Workspace []config.AgentFile
	// Dirs are the operator's directories; a later one wins a name.
	Dirs []string
	// Disabled loads the managed definitions only.
	Disabled bool
	// Models are the provider names a definition may choose. A definition
	// naming any other is refused.
	Models []string
}

// Load reads every definition. A file that does not load is reported and
// skipped; the rest still load.
func Load(o Options) ([]*agent.Definition, []error) {
	type level struct {
		source string
		files  []file
	}
	var levels []level
	var errs []error
	// Every name a managed file claims is the organisation's, whether or not
	// that file loads here: a managed role that cannot run must not be
	// replaced by another file under its name.
	managedClaim := map[string]string{}
	if o.ManagedDir != "" {
		fs, claims, unlistable, e := readManaged(o.ManagedDir)
		levels = append(levels, level{agent.SourceManaged, fs})
		errs = append(errs, e...)
		for name, path := range claims {
			managedClaim[name] = path
		}
		if unlistable {
			// Which names the organisation holds cannot be known, so no
			// other file may take any: only the built-in roles remain.
			errs = append(errs, fmt.Errorf("agent definitions: %s exists but cannot be listed, so no workspace or operator definition is loaded", config.Printable(o.ManagedDir)))
			o.Disabled, o.Workspace = true, nil
		}
	}
	if !o.Disabled {
		var ws []file
		for _, f := range o.Workspace {
			ws = append(ws, file{path: f.Path, data: f.Data})
		}
		levels = append(levels, level{agent.SourceWorkspace, ws})
		// Later operator directories win, so they are read first here.
		var op []file
		for i := len(o.Dirs) - 1; i >= 0; i-- {
			fs, e := readDir(expandHome(o.Dirs[i]))
			op = append(op, fs...)
			errs = append(errs, e...)
		}
		levels = append(levels, level{agent.SourceOperator, op})
	}

	byName := map[string]*agent.Definition{}
	seen := map[string]bool{} // files already loaded, so a directory named twice loads once
	var out []*agent.Definition
	for _, lv := range levels {
		for _, f := range lv.files {
			key := realPath(f.path)
			if seen[key] {
				continue
			}
			seen[key] = true
			if lv.source == agent.SourceManaged {
				managedClaim[claimedName(f.path, f.data)] = f.path
			} else if owner, claimed := managedClaim[claimedName(f.path, f.data)]; claimed {
				if _, loaded := byName[claimedName(f.path, f.data)]; !loaded {
					errs = append(errs, fmt.Errorf("agent definition %s refused: the name %q belongs to the organisation's %s, which did not load",
						config.Printable(f.path), claimedName(f.path, f.data), config.Printable(owner)))
					continue
				}
			}
			def, warns, err := Parse(f.path, f.data, lv.source, o.Models)
			for _, w := range warns {
				errs = append(errs, fmt.Errorf("agent definition %s: %s", config.Printable(f.path), w))
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("agent definition %s refused: %w", config.Printable(f.path), err))
				continue
			}
			if prev, taken := byName[def.Name]; taken {
				errs = append(errs, fmt.Errorf("agent definition %s is shadowed by %s (%s), which defines %q too",
					config.Printable(f.path), config.Printable(prev.Path), prev.Source, def.Name))
				continue
			}
			byName[def.Name] = def
			out = append(out, def)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

type file struct {
	path string
	data []byte
}

// claimedName is the name a file would define: its name key when the header
// reads, its file name otherwise. A managed file claims it even when refused.
func claimedName(path string, data []byte) string {
	if doc, err := frontmatter.Parse(string(data)); err == nil {
		for _, f := range doc.Top() {
			if keyOf(f.Key) == "name" && strings.TrimSpace(f.Value) != "" {
				return strings.TrimSpace(f.Value)
			}
		}
	}
	return strings.TrimSuffix(filepath.Base(path), ".md")
}

// ManagedOwnerOK decides whether a managed definition reached through a
// symlink may be followed: the target must be owned by root and writable by
// nobody else. A variable so a test, which does not run as root, can say.
var ManagedOwnerOK = rootOwnedNotShared

// readManaged reads the organisation's directory. Every *.md entry claims
// its file name, and a file that loads claims its name key too, whether or
// not either loads in the end. A directory that exists but cannot be listed
// is reported as unlistable, and the caller fails closed.
func readManaged(dir string) (files []file, claims map[string]string, unlistable bool, errs []error) {
	claims = map[string]string{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, claims, false, nil
	}
	if err != nil {
		return nil, claims, true, []error{fmt.Errorf("agent definitions: read %s: %w", config.Printable(dir), err)}
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		claims[strings.TrimSuffix(name, ".md")] = path
		data, err := readManagedFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent definition %s refused, and its name stays the organisation's: %w", config.Printable(path), err))
			continue
		}
		claims[claimedName(path, data)] = path
		files = append(files, file{path: path, data: data})
	}
	return files, claims, false, errs
}

// readManagedFile reads a managed definition. A symlink, as configuration
// management installs them, is followed only to a regular file owned by
// root and writable by nobody else.
func readManagedFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return config.ReadAgentFile(path)
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	tinfo, err := os.Lstat(target)
	if err != nil {
		return nil, err
	}
	if !tinfo.Mode().IsRegular() || !ManagedOwnerOK(tinfo) {
		return nil, fmt.Errorf("a link is followed only to a regular file owned by root and writable by nobody else")
	}
	return config.ReadAgentFile(target)
}

// readDir reads a directory's *.md files, sorted. A missing directory is not
// an error: naming one before writing the first definition is reasonable.
func readDir(dir string) ([]file, []error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{fmt.Errorf("agent definitions: read %s: %w", config.Printable(dir), err)}
	}
	var out []file
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := config.ReadAgentFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent definition %s refused: %w", config.Printable(path), err))
			continue
		}
		out = append(out, file{path: path, data: data})
	}
	return out, errs
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// keyOf folds the spellings of one key: permissionMode, permission_mode and
// permission-mode are the same key.
// A quoted key is the same key: "tools" and tools must read alike, or a
// quoted restriction would be ignored.
func keyOf(k string) string {
	k = strings.TrimSpace(k)
	if len(k) >= 2 && (k[0] == '"' && k[len(k)-1] == '"' || k[0] == '\'' && k[len(k)-1] == '\'') {
		k = strings.TrimSpace(k[1 : len(k)-1])
	}
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(k))
}

// honoured are the keys a definition may set, folded.
var honoured = map[string]bool{"name": true, "description": true, "tools": true, "disallowedtools": true,
	"model": true, "maxturns": true, "isolation": true, "permissionmode": true}

// cosmetic keys change nothing Abhed enforces, so they are ignored with a warning.
var cosmetic = map[string]bool{"color": true, "colour": true, "icon": true, "emoji": true}

// refusedKey says why a key refuses the definition: it would concern
// authority, and Abhed does not honour it.
func refusedKey(k string) string {
	switch {
	case k == "disallowedtools":
		return ""
	case k == "hooks":
		return "hooks are configured by the operator, not by a definition"
	case k == "mcpservers":
		return "MCP servers are configured by the operator, not by a definition"
	case k == "permissions":
		return "permission rules are configured by the operator, not by a definition"
	case strings.Contains(k, "allow"), strings.Contains(k, "deny"):
		return "a definition may only list tools and disallowed_tools"
	case strings.Contains(k, "sandbox"), strings.Contains(k, "network"), k == "env", strings.Contains(k, "readonly"):
		return "the sandbox is configured by the operator, not by a definition"
	}
	// A key that is not one of ours but reads like one was probably meant as
	// one: denied_tools, permission, max_turn. Ignoring it could leave a
	// restriction out, so it refuses.
	if !honoured[k] {
		for _, like := range []string{"tool", "mode", "model", "turn", "permission", "exclude", "block", "restrict"} {
			if strings.Contains(k, like) {
				return "it reads like a restriction, and only tools, disallowed_tools, model, max_turns and permission_mode are honoured"
			}
		}
	}
	return ""
}

// Parse reads one definition file. Warnings are about keys ignored; an error
// refuses the definition.
func Parse(path string, data []byte, source string, models []string) (*agent.Definition, []string, error) {
	if !utf8.Valid(data) {
		return nil, nil, fmt.Errorf("not UTF-8 text")
	}
	doc, err := frontmatter.Parse(string(data))
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(data)
	def := &agent.Definition{Source: source, Path: path, SHA256: hex.EncodeToString(sum[:])}
	var warns []string
	// A key nested under another is checked as a top-level one is: a
	// restriction inside a settings: block must not pass as ignored.
	for _, f := range doc.Fields {
		if !f.Nested {
			continue
		}
		why := refusedKey(keyOf(f.Key))
		if honoured[keyOf(f.Key)] {
			why = "it is honoured only at the top level"
		}
		if why != "" {
			return nil, warns, fmt.Errorf("nested key %q is not honoured: %s. A definition that expected it would run looser than its author meant", config.Printable(f.Key), why)
		}
	}
	set := map[string]bool{}
	for _, f := range doc.Top() {
		k := keyOf(f.Key)
		if set[k] {
			return nil, warns, fmt.Errorf("%q is set twice", f.Key)
		}
		set[k] = true
		if why := refusedKey(k); why != "" {
			return nil, warns, fmt.Errorf("%q is not honoured: %s. A definition that expected it would run looser than its author meant", config.Printable(f.Key), why)
		}
		switch k {
		case "name", "description", "model", "maxturns", "isolation", "permissionmode":
			if f.Kind != frontmatter.Scalar {
				return nil, warns, fmt.Errorf("%q must be a single value", f.Key)
			}
		case "tools", "disallowedtools":
			if f.Kind == frontmatter.Map {
				return nil, warns, fmt.Errorf("%q must be a list of tool names", f.Key)
			}
		}
		switch k {
		case "name":
			def.Name = f.Value
		case "description":
			def.Description = strings.Join(strings.Fields(f.Value), " ")
		case "tools":
			if def.Tools, err = toolList(f); err != nil {
				return nil, warns, fmt.Errorf("tools: %w", err)
			}
		case "disallowedtools":
			if def.DisallowedTools, err = toolList(f); err != nil {
				return nil, warns, fmt.Errorf("%s: %w", f.Key, err)
			}
		case "model":
			if def.Model, err = modelOf(f.Value, models); err != nil {
				return nil, warns, err
			}
		case "maxturns":
			n, err := strconv.Atoi(f.Value)
			if err != nil || n < 1 || n > MaxTurns {
				return nil, warns, fmt.Errorf("%s must be a whole number from 1 to %d", f.Key, MaxTurns)
			}
			def.MaxTurns = n
		case "isolation":
			switch f.Value {
			case "none", "":
			case "worktree":
				def.Isolation = "worktree"
			default:
				return nil, warns, fmt.Errorf("isolation must be none or worktree")
			}
		case "permissionmode":
			switch f.Value {
			case "plan", "default":
				def.PermissionMode = f.Value
			default:
				// Anything wider than default would ask for more than the
				// session may have; say so rather than run it as default.
				return nil, warns, fmt.Errorf("%s may only be plan or default, not %q", f.Key, config.Printable(f.Value))
			}
		default:
			if cosmetic[k] {
				warns = append(warns, fmt.Sprintf("%q is ignored", config.Printable(f.Key)))
			} else {
				warns = append(warns, fmt.Sprintf("%q is not a definition key and is ignored", config.Printable(f.Key)))
			}
		}
	}
	if def.Name == "" {
		def.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	if !nameRE.MatchString(def.Name) {
		return nil, warns, fmt.Errorf("name %q must be lower case letters, digits and dashes, starting with a letter, at most 40", config.Printable(def.Name))
	}
	if agent.Reserved(def.Name) {
		return nil, warns, fmt.Errorf("%q is a built-in agent type and cannot be redefined", def.Name)
	}
	if def.Description == "" {
		return nil, warns, fmt.Errorf("a description saying when to use this agent is required")
	}
	if n := utf8.RuneCountInString(def.Description); n > MaxDescription {
		return nil, warns, fmt.Errorf("the description is %d characters; at most %d", n, MaxDescription)
	}
	for _, r := range def.Description {
		if !unicode.IsPrint(r) {
			return nil, warns, fmt.Errorf("the description holds a control character")
		}
	}
	if strings.TrimSpace(doc.Body) == "" {
		return nil, warns, fmt.Errorf("the role instructions after the frontmatter are empty")
	}
	if len(doc.Body) > MaxBody {
		return nil, warns, fmt.Errorf("the role instructions are %d bytes; at most %d", len(doc.Body), MaxBody)
	}
	def.Instruction = doc.Body
	return def, warns, nil
}

// toolList reads tools written as a comma string, an inline list or items.
func toolList(f frontmatter.Field) ([]string, error) {
	items := f.List
	if f.Kind == frontmatter.Scalar {
		items = strings.Split(f.Value, ",")
	}
	out := []string{}
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if !toolRE.MatchString(it) {
			return nil, fmt.Errorf("%q is not a tool name; the only wildcard is mcp__<server>__*", config.Printable(it))
		}
		out = append(out, it)
	}
	return out, nil
}

// modelOf checks a model choice is a configured provider name, never an
// endpoint. inherit, or nothing, runs on the parent's model.
func modelOf(v string, models []string) (string, error) {
	if v == "" || v == "inherit" {
		return "", nil
	}
	if err := agent.ValidModelName(v); err != nil {
		return "", err
	}
	for _, m := range models {
		if m == v {
			return v, nil
		}
	}
	avail := "none offers a choice"
	if len(models) > 0 {
		avail = strings.Join(models, ", ")
	}
	return "", fmt.Errorf("model %q is not a configured provider; available: %s", config.Printable(v), avail)
}
