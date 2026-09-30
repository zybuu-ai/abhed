package ui

import "fmt"

// Answers to a plan card.
const (
	// PlanAcceptEdits approves the plan and switches to accept-edits.
	PlanAcceptEdits = "accept-edits"
	// PlanApprove approves the plan and keeps asking before each change.
	PlanApprove = "approve"
	// PlanKeep keeps planning: the agent goes on exploring, in plan mode.
	PlanKeep = "keep-planning"
	// PlanRefine keeps plan mode and lets the person say what to change.
	PlanRefine = "refine"
)

// PlanCard is the question put when the agent has finished a plan in plan
// mode: the plan itself, and what to do with it. Nothing is selected until
// a choice is made, and Esc means "tell Abhed what to change". Whoever asks
// records the answer (plan.decided) and changes the mode through the
// ModeController; the card only collects it.
func PlanCard(plan string) DialogSpec {
	return DialogSpec{
		Kind:  DialogChoice,
		Title: "Plan ready",
		Why:   "plan mode: nothing has been changed yet",
		Body:  []Block{{Kind: BlockMarkdown, Text: plan}},
		Ask:   "Proceed with this plan?",
		Choices: []Choice{
			{ID: PlanAcceptEdits, Label: "Yes, and accept edits without asking", Widening: true},
			{ID: PlanApprove, Label: "Yes, and ask before each change"},
			{ID: PlanKeep, Label: "No, keep planning"},
			{ID: PlanRefine, Label: "No, and tell Abhed what to change (esc)"},
		},
		Cancel: PlanRefine,
		Outcome: func(id string) string {
			switch id {
			case PlanAcceptEdits:
				return "Plan approved · edits accepted without asking"
			case PlanApprove:
				return "Plan approved · each change is asked about"
			case PlanKeep:
				return "Keep planning"
			}
			return "Refine the plan"
		},
	}
}

// ModeConfirm is the question a switch to auto mode needs: Shift-Tab never
// reaches it, and /mode auto asks first, with No as the answer Enter gives.
func ModeConfirm(from, to string) DialogSpec {
	what := "commands and edits run without asking, within the policy's rules"
	if to == "auto" {
		what = "edits and read-only tools run without asking; commands still ask, and destructive ones always do"
	}
	return DialogSpec{
		Kind:  DialogConfirm,
		Title: fmt.Sprintf("Switch to %s mode?", to),
		Why:   "in " + to + " mode " + what,
		Ask:   fmt.Sprintf("Leave %s mode for %s?", from, to),
		Choices: []Choice{
			{ID: ChoiceNo, Label: "No, stay in " + from + " mode", Key: 'n'},
			{ID: ChoiceYes, Label: "Yes, switch to " + to + " mode", Key: 'y', Widening: true},
		},
		Default: ChoiceNo,
		Cancel:  ChoiceNo,
	}
}
