package agent

import (
	"sort"
	"strings"
)

// Definition is one subagent type the model may ask for: a built-in role, or
// one loaded from a markdown definition file. Everything in it may only
// narrow what the parent could do; it carries no approver and no rule.
type Definition struct {
	// Name is the agent_type value.
	Name string
	// Description tells the model when to use it.
	Description string
	// Instruction is the role text, the Role section of the child's prompt.
	Instruction string
	// Tools, when non-nil, is the allow-list the child's tools are cut to.
	// Nil is the parent's set. Entries are matched case-folded; mcp__<server>__*
	// is the one wildcard.
	Tools []string
	// DisallowedTools are removed after Tools.
	DisallowedTools []string
	// Model names a configured provider; empty inherits the parent's model.
	Model string
	// MaxTurns caps the child's turns; zero keeps the default cap.
	MaxTurns int
	// Isolation is "worktree" for a role that works in its own checkout.
	Isolation string
	// PermissionMode is "plan" or "default", honoured only where it narrows.
	PermissionMode string
	// Source is builtin, managed, workspace or operator.
	Source string
	// Path is the file it was read from; SHA256 is that file's content hash.
	Path   string
	SHA256 string

	// strict marks a loaded definition: a tool it names that the session
	// does not have refuses the spawn. The built-ins keep their fixed lists.
	strict bool
}

// Definition sources, as the record names them.
const (
	SourceBuiltin   = "builtin"
	SourceManaged   = "managed"
	SourceWorkspace = "workspace"
	SourceOperator  = "operator"
)

// ReservedNames are the built-in roles' names. A file cannot redefine them:
// a repository changing what "explore" means changes what the model believes
// it is asking for.
var ReservedNames = []string{"main", "general", "explore", "test", "review"}

// Reserved reports whether name belongs to a built-in role.
func Reserved(name string) bool {
	for _, r := range ReservedNames {
		if r == name {
			return true
		}
	}
	return false
}

// Definitions is the set of subagent types one session offers, fixed for the
// session: it is part of the cached prompt prefix and of what the record says
// the model was offered.
type Definitions struct {
	byName map[string]*Definition
}

// builtinDescriptions say when to use each built-in role.
var builtinDescriptions = map[string]string{
	"general": "any self-contained subtask, with the session's tools",
	"explore": "locate and summarize code; reads only",
	"test":    "run tests, diagnose failures and fix them",
	"review":  "review for correctness bugs; reports, does not fix",
}

// BuiltinDefinitions is the set with only the built-in roles.
func BuiltinDefinitions() *Definitions {
	d := &Definitions{byName: map[string]*Definition{}}
	for name, desc := range builtinDescriptions {
		profile := name
		if name == "general" {
			profile = "main"
		}
		p := Profiles[profile]
		d.byName[name] = &Definition{Name: name, Description: desc, Instruction: p.Instruction,
			Tools: p.Tools, Source: SourceBuiltin}
	}
	return d
}

// WithDefinitions adds loaded definitions to the built-ins. A reserved name is
// skipped: the loader refuses those, and this holds if it did not.
func WithDefinitions(defs ...*Definition) *Definitions {
	d := BuiltinDefinitions()
	for _, def := range defs {
		if def == nil || Reserved(def.Name) {
			continue
		}
		c := *def
		c.strict = true
		d.byName[c.Name] = &c
	}
	return d
}

// Get finds a type by name. "main" is the general role's older name.
func (d *Definitions) Get(name string) (*Definition, bool) {
	if d == nil {
		d = BuiltinDefinitions()
	}
	if name == "" || name == "main" {
		name = "general"
	}
	def, ok := d.byName[name]
	return def, ok
}

// Names lists the types in the order the model is shown them: the built-ins,
// then the loaded ones sorted.
func (d *Definitions) Names() []string {
	if d == nil {
		d = BuiltinDefinitions()
	}
	out := []string{"general", "explore", "test", "review"}
	var custom []string
	for name, def := range d.byName {
		if def.Source != SourceBuiltin {
			custom = append(custom, name)
		}
	}
	sort.Strings(custom)
	return append(out, custom...)
}

// Loaded are the definitions read from files, sorted by name.
func (d *Definitions) Loaded() []*Definition {
	if d == nil {
		return nil
	}
	var out []*Definition
	for _, def := range d.byName {
		if def.Source != SourceBuiltin {
			out = append(out, def)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// listing is the lines naming each type and when to use it.
func (d *Definitions) listing() string {
	var b strings.Builder
	for _, name := range d.Names() {
		def, _ := d.Get(name)
		b.WriteString("\n- ")
		b.WriteString(name)
		b.WriteString(" — ")
		b.WriteString(def.Description)
	}
	return b.String()
}
