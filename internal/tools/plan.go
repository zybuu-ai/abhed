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

// ExitPlan is how the agent, in plan mode, puts its plan to the person. It
// changes nothing and cannot change the mode: it hands the plan to Submit
// and ends the run, and the person's answer decides whether the session
// leaves plan mode, and into which mode.
type ExitPlan struct {
	// Submit records the plan and keeps it for the person's decision.
	Submit func(ctx context.Context, plan string) error
}

// ExitPlanName is the tool's name.
const ExitPlanName = "exit_plan"

func (ExitPlan) Name() string       { return ExitPlanName }
func (ExitPlan) Mutates() bool      { return false }
func (ExitPlan) OnlyInMode() string { return "plan" }

func (ExitPlan) Description() string {
	return "Present your plan to the user when it is ready, in plan mode. Write it in markdown: the steps, " +
		"the files you will change and how you will check the result. The user decides whether you may " +
		"carry it out; stop after calling this and change nothing until they answer."
}

func (ExitPlan) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "plan":{"type":"string","description":"The plan, in markdown."}
  },
  "required":["plan"]
}`)
}

func (t ExitPlan) Run(ctx context.Context, _ *Session, raw json.RawMessage) Result {
	var a struct {
		Plan string `json:"plan"`
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
	if err := t.Submit(ctx, a.Plan); err != nil {
		return errf("The plan was not presented: %v", err)
	}
	return Result{Content: "The plan was presented to the user. Stop here and change nothing until they answer.", Final: true}
}
