package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/jsonschema"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Structured output: a run whose answer is a JSON value matching a schema.
//
// Done at the harness level rather than per provider, and deliberately so.
// Every one of the twenty providers can call a tool; only a few have a native
// "respond in this shape" mode, and those disagree with each other. So the
// answer is delivered by calling a tool named `result` whose input schema IS
// the caller's schema. The tool validates, and either accepts — ending the
// run, because the answer has been given — or rejects with a path-by-path
// list of what is wrong, which the model reads and fixes on its next turn.
// The caller receives validated JSON or an error, never prose to parse.

// ErrNoResult is returned when the run ended without the model delivering an
// answer through the result tool. The reason says how it ended instead.
type ErrNoResult struct {
	Reason TerminalReason
	Last   string // the model's last message, for the caller's diagnostics
}

func (e ErrNoResult) Error() string {
	return fmt.Sprintf("run ended (%s) without delivering a result", e.Reason)
}

// ResultToolName is what the model calls to deliver a structured answer.
const ResultToolName = "result"

// resultTool captures one validated answer.
type resultTool struct {
	schema   json.RawMessage
	compiled *jsonschema.Schema

	mu       sync.Mutex
	got      json.RawMessage
	attempts int
}

func (r *resultTool) Name() string  { return ResultToolName }
func (r *resultTool) Mutates() bool { return false }
func (r *resultTool) Description() string {
	return "Deliver the final answer. Call this exactly once, with an object that " +
		"matches the schema. Nothing else you write is returned to the caller — only " +
		"this call is. If it is rejected, fix the fields named and call it again."
}
func (r *resultTool) Schema() json.RawMessage { return r.schema }

// MaxResultAttempts bounds how many times a rejected answer may be retried
// before the run is ended. A model that cannot satisfy the schema in five
// attempts is not going to on the sixth, and each attempt costs a turn.
const MaxResultAttempts = 5

func (r *resultTool) Run(ctx context.Context, _ *tools.Session, args json.RawMessage) tools.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if err := r.compiled.Validate(args); err != nil {
		if r.attempts >= MaxResultAttempts {
			return tools.Result{
				Content: "The result does not match the schema:\n" + err.Error() +
					"\n\nThat was the last attempt. Stopping.",
				IsError: true, Final: true,
			}
		}
		return tools.Result{
			Content: "The result does not match the schema:\n" + err.Error() +
				"\n\nFix the fields named above and call result again with the complete object.",
			IsError: true,
		}
	}
	r.got = append(json.RawMessage(nil), args...)
	return tools.Result{Content: "Result accepted.", Final: true}
}

// RunStructured runs a prompt and returns the JSON the model delivered
// through the result tool, validated against schema.
//
// The registry is modified for the duration of the call — the result tool is
// added and then removed — so a registry shared with another running loop
// would see a tool appear. Callers pass a per-agent registry.
func RunStructured(ctx context.Context, loop *Loop, registry *tools.Registry,
	prompt string, schema json.RawMessage) (json.RawMessage, TerminalReason, error) {

	compiled, err := jsonschema.Compile(schema)
	if err != nil {
		return nil, "", fmt.Errorf("output schema: %w", err)
	}
	if _, taken := registry.Get(ResultToolName); taken {
		return nil, "", errors.New("a tool named \"result\" is already registered")
	}
	rt := &resultTool{schema: schema, compiled: compiled}
	registry.Add(rt)
	defer registry.Remove(ResultToolName)
	// The result is the answer here: the answer tool, which would end the run
	// without one, is put away for the run.
	if at, ok := registry.Get(AnswerToolName); ok {
		registry.Remove(AnswerToolName)
		defer registry.Add(at)
	}

	// The instruction rides with the prompt rather than the system prompt so
	// it applies to this run only, and so a caller's own system prompt is not
	// edited underneath them.
	prompt = strings.TrimRight(prompt, "\n") + "\n\n" +
		"Deliver your final answer by calling the `result` tool with an object that " +
		"matches its schema. That call is the only output the caller receives; do not " +
		"restate the answer as text."

	reason, err := loop.Run(ctx, prompt)
	if err != nil {
		return nil, reason, err
	}
	rt.mu.Lock()
	got := rt.got
	rt.mu.Unlock()
	if got == nil {
		return nil, reason, ErrNoResult{Reason: reason, Last: lastAssistantMessage(loop.Messages())}
	}
	return got, reason, nil
}
