package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Tool loads a skill's instructions.
//
// The whole point of a tool rather than prompt inclusion is cost: the listing
// in the system prompt is a name and one line each, and the body — which may
// run to thousands of tokens — is paid only in the sessions that actually use
// it.
type Tool struct {
	R *Registry
	// RunPipeline executes a skill's declared pipeline. When nil, or when a
	// skill declares none, the tool returns the instructions as before.
	//
	// The seam is here rather than inside this package so that skills stays
	// free of the model adapter, the tool registry and the policy engine: a
	// pipeline step is still an ordinary tool call, made by the caller, through
	// everything an ordinary tool call goes through.
	RunPipeline func(ctx context.Context, skill *Skill, input string) (string, error)
	// Input is the user's request, which a pipeline needs and instructions do
	// not. Set per turn by the caller.
	Input func() string
}

func (Tool) Name() string { return "skill" }

// NarrowSkills is the tool offering only names, and the names it does not
// have, for a subagent role limited to some skills.
func (t Tool) NarrowSkills(names []string) (tools.Tool, []string) {
	cut := NewRegistry()
	var missing []string
	for _, n := range names {
		var s *Skill
		ok := false
		if t.R != nil {
			s, ok = t.R.Get(n)
		}
		if !ok {
			missing = append(missing, n)
			continue
		}
		cut.add(s)
	}
	t.R = cut
	return t, missing
}

// Mutates is false. Reading instructions changes nothing; whatever the
// instructions then tell the agent to do goes through the ordinary permission
// checks for those tools. Marking this as mutating would prompt the user to
// approve reading a file, which trains them to click through prompts.
func (Tool) Mutates() bool { return false }

func (t Tool) Description() string {
	names := ""
	if t.R != nil && t.R.Len() > 0 {
		names = " Available: " + strings.Join(t.R.Names(), ", ") + "."
	}
	// Models matched a skill and worked unaided; it is to be the first call.
	return "Load the full instructions for a named skill." + names +
		" When a request matches a skill listed in the system prompt, call this first, " +
		"before reading files or searching, then follow the instructions it returns."
}

func (Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "name":{"type":"string","description":"The skill to load, exactly as listed."}
  },
  "required":["name"]
}`)
}

type args struct {
	Name string `json:"name"`
}

func (t Tool) Run(ctx context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for skill: %v", err)
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return errf("name is required.")
	}
	if t.R == nil || t.R.Len() == 0 {
		return errf("No skills are configured on this deployment.")
	}

	s, ok := t.R.Get(name)
	if !ok {
		// Naming the alternatives ends the retry loop that a bare "not found"
		// otherwise causes.
		return errf("No skill named %q. Available: %s.",
			name, strings.Join(t.R.Names(), ", "))
	}

	// A skill that declares a pipeline is executed rather than described. The
	// model gets what the pipeline gathered and the job of writing the answer;
	// it no longer has to remember a seven-step procedure, which is the thing
	// it demonstrably does not do reliably.
	if len(s.Pipeline) > 0 && t.RunPipeline != nil {
		input := ""
		if t.Input != nil {
			input = t.Input()
		}
		out, err := t.RunPipeline(ctx, s, input)
		if err != nil {
			// Fall back to the instructions. A pipeline that cannot run is a
			// worse outcome than one that never existed, and the skill still
			// describes what to do.
			return tools.Result{Content: fmt.Sprintf(
				"The %s pipeline could not complete (%v), so here are its "+
					"instructions to follow directly.\n\n%s",
				s.Name, err, s.Body)}
		}
		return tools.Result{Content: out}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Skill: %s\n", s.Name)
	if s.Dir != "" {
		// Instructions routinely reference files beside them, and many skills
		// written for other harnesses use a $SKILL_DIR placeholder rather than
		// a relative path. Give both the value and the substitution rule: the
		// model has no shell that expands it, so an instruction saying
		// "bash $SKILL_DIR/scripts/run.sh" would otherwise run /scripts/run.sh
		// and fail.
		fmt.Fprintf(&b, "Skill directory: %s\n", s.Dir)
		fmt.Fprintf(&b, "When these instructions write $SKILL_DIR or "+
			"${SKILL_DIR}, substitute %s.\n", s.Dir)
	}
	b.WriteString("\n")
	b.WriteString(s.Body)
	return tools.Result{Content: b.String()}
}

func errf(format string, a ...any) tools.Result {
	return tools.Result{Content: fmt.Sprintf(format, a...), IsError: true}
}
