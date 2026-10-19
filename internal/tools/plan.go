package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// ModeScoped is a tool offered only in one permission mode. The loop leaves
// it out of the model's tool list, and refuses a call to it, in any other.
type ModeScoped interface {
	OnlyInMode() string
}

// RunbookStep is one step of a plan's runbook: a tool call the person
// approves with the plan, named by tool, where it runs and what it runs.
// Target is a host for ssh and cluster/namespace for k8s_apply; Command,
// Manifest and Args fill the rest of the call's arguments.
type RunbookStep struct {
	Tool     string          `json:"tool"`
	Target   string          `json:"target,omitempty"`
	Command  string          `json:"command,omitempty"`
	Manifest string          `json:"manifest,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	Note     string          `json:"note,omitempty"`
}

// ExitPlan is how the agent, in plan mode, puts its plan to the person. It
// changes nothing and cannot change the mode: it hands the plan to Submit
// and ends the run, and the person's answer decides whether the session
// leaves plan mode, and into which mode.
type ExitPlan struct {
	// Submit records the plan and keeps it for the person's decision. An
	// error refuses the plan, and the model is told why.
	Submit func(ctx context.Context, plan string, runbook []RunbookStep) error
}

// ExitPlanName is the tool's name.
const ExitPlanName = "exit_plan"

func (ExitPlan) Name() string       { return ExitPlanName }
func (ExitPlan) Mutates() bool      { return false }
func (ExitPlan) OnlyInMode() string { return "plan" }

func (ExitPlan) Description() string {
	return "Present your plan to the user when it is ready, in plan mode. Write it in markdown: the steps, " +
		"the files you will change and how you will check the result. For remote or operational work, also " +
		"give a runbook: the exact tool calls, in order. The user approves it once; afterwards each call that " +
		"matches the next step exactly runs without asking, and anything else asks again. The user decides " +
		"whether you may carry it out; stop after calling this and change nothing until they answer."
}

func (ExitPlan) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "plan":{"type":"string","description":"The plan, in markdown."},
    "runbook":{"type":"array","description":"Optional. The exact calls the plan makes, in the order they will run. Make each call later with exactly these arguments.",
      "items":{"type":"object","properties":{
        "tool":{"type":"string","enum":["ssh","k8s_apply","bash","edit","write"],"description":"The tool the step calls."},
        "target":{"type":"string","description":"ssh: the host. k8s_apply: cluster/namespace (cluster alone for a cluster-wide resource)."},
        "command":{"type":"string","description":"ssh and bash: the command, exactly as it will be run."},
        "manifest":{"type":"string","description":"k8s_apply: the manifest, for action apply."},
        "args":{"type":"object","description":"Any other arguments of the call, as the tool takes them (k8s_apply action, resource, name, replicas; edit and write path, old_string, new_string, content)."},
        "note":{"type":"string","description":"What the step is for, in a few words."}
      },"required":["tool"]}}
  },
  "required":["plan"]
}`)
}

func (t ExitPlan) Run(ctx context.Context, _ *Session, raw json.RawMessage) Result {
	var a struct {
		Plan    string        `json:"plan"`
		Runbook []RunbookStep `json:"runbook"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errf("Invalid arguments for exit_plan: %v", err)
	}
	if strings.TrimSpace(a.Plan) == "" {
		return errf("The plan is empty. Call exit_plan with the plan itself.")
	}
	if t.Submit == nil {
		return errf("Plans cannot be presented in this session.")
	}
	if err := t.Submit(ctx, a.Plan, a.Runbook); err != nil {
		return errf("The plan was not presented: %v", err)
	}
	return Result{Content: "The plan was presented to the user. Stop here and change nothing until they answer.", Final: true}
}
