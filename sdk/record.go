package abhed

import (
	"context"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// The record's vocabulary. Anything that reads the audit log outside this
// module — an exporter, a compliance tool, a dashboard — needs the same
// names the harness writes, and cannot reach them under internal/. These
// aliases are that contract: the types are identical, not copies, so a
// value from the harness needs no conversion, and a rename here would be a
// breaking change and is treated as one.

// EventType names what happened; Actor names who did it.
type (
	EventType      = agent.EventType
	Actor          = agent.Actor
	Trust          = agent.Trust
	TerminalReason = agent.TerminalReason
)

const (
	EvSessionStarted  = agent.EvSessionStarted
	EvUserMessage     = agent.EvUserMessage
	EvAgentMessage    = agent.EvAgentMessage
	EvAgentDelta      = agent.EvAgentDelta
	EvAgentReasoning  = agent.EvAgentReasoning
	EvActionRequested = agent.EvActionRequested
	EvActionApproved  = agent.EvActionApproved
	EvActionDenied    = agent.EvActionDenied
	EvObservation     = agent.EvObservation
	EvSubagentSpawned = agent.EvSubagentSpawned
	EvSubagentReturn  = agent.EvSubagentReturn
	EvCompactStarted  = agent.EvCompactStarted
	EvCompactDone     = agent.EvCompactDone
	EvPlanUpdated     = agent.EvPlanUpdated
	EvTodoUpdated     = agent.EvTodoUpdated
	EvSessionEnded    = agent.EvSessionEnded
	EvTerminalInput   = agent.EvTerminalInput
	EvMessageDropped  = agent.EvMessageDropped

	EvAgentReasoningDelta = agent.EvAgentReasoningDelta

	ActorUser   = agent.ActorUser
	ActorAgent  = agent.ActorAgent
	ActorSystem = agent.ActorSystem
	ActorTool   = agent.ActorTool

	Trusted   = agent.Trusted
	Untrusted = agent.Untrusted

	TermCompleted     = agent.TermCompleted
	TermMaxTurns      = agent.TermMaxTurns
	TermMaxBudget     = agent.TermMaxBudget
	TermPolicyDenied  = agent.TermPolicyDenied
	TermUserInterrupt = agent.TermUserInterrupt
	TermError         = agent.TermError
	TermShutdown      = agent.TermShutdown
	TermDeadline      = agent.TermDeadline
)

// Event payloads, one per kind that carries structure.
type (
	ActionRequested = agent.ActionRequested
	Observation     = agent.Observation
	Message         = agent.Message
	Delta           = agent.Delta
	Reasoning       = agent.Reasoning
	SessionEnded    = agent.SessionEnded
	Todo            = agent.Todo
	TodoList        = agent.TodoList
	Compaction      = agent.Compaction
	DroppedMessage  = agent.DroppedMessage
)

// Who settled a call, as action.approved and action.denied record it in "by".
const (
	ByPolicy       = agent.ByPolicy
	ByReviewer     = agent.ByReviewer
	ByUser         = agent.ByUser
	BySessionScope = agent.BySessionScope
	ByHeadless     = agent.ByHeadless
	BySystem       = agent.BySystem
)

// Answer is what an approver reports when someone other than the person it
// would ask settled a request, such as a scope it remembered.
type Answer struct {
	By     string // one of the By values
	Scope  string // the remembered scope that allowed it, for BySessionScope
	Reason string // why no one answered, said in place of "rejected"
	// Approver names the person who answered, recorded as "approver" as the
	// embedder asserts it: Abhed does not verify it.
	Approver string
	// Granted is the scope the person chose to always allow, recorded as "granted_scope".
	Granted string
}

// NoteAnswer reports a from inside Options.Approve, with the ctx it was
// given. Without it the record says a reviewer answered. A By that is not
// one of the By values is ignored.
func NoteAnswer(ctx context.Context, a Answer) {
	agent.NoteAnswer(ctx, agent.Answer{By: a.By, Scope: a.Scope, Reason: a.Reason, Approver: a.Approver, Granted: a.Granted})
}

// Rule is one permission rule as written in configuration, and ParseRule
// reads one, so a tool that validates a policy file applies the same parser
// the harness does.
type Rule = policy.Rule

func ParseRule(s string) (Rule, error) { return policy.ParseRule(s) }
