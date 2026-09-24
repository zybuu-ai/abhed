package abhed

import (
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
)

// Rule is one permission rule as written in configuration, and ParseRule
// reads one, so a tool that validates a policy file applies the same parser
// the harness does.
type Rule = policy.Rule

func ParseRule(s string) (Rule, error) { return policy.ParseRule(s) }
