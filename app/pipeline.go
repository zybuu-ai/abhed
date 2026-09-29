package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/pipeline"
	"github.com/zybuu-ai/abhed/internal/skills"
)

// pipelineRunner builds the function the skill tool calls to execute a
// declared pipeline.
//
// A tool step is put through the loop that called the skill, as its own call is:
// policy, hooks, the monitor, the approver, the sandbox, redaction and the
// record. A model step is a completion on the configured adapter, and sees
// tool output only once secrets are stripped from it. The pipeline decides
// what happens and in what order; it does not decide what is permitted.
func pipelineRunner(adapter model.Adapter) func(context.Context, *skills.Skill, string) (string, error) {

	return func(ctx context.Context, s *skills.Skill, input string) (string, error) {
		var p pipeline.Pipeline
		if err := json.Unmarshal(s.Pipeline, &p); err != nil {
			return "", fmt.Errorf("pipeline.json: %w", err)
		}
		if err := p.Validate(); err != nil {
			return "", err
		}
		if err := refuseUnsafe(p); err != nil {
			return "", err
		}
		// Steps run on the loop that called the skill, or the pipeline is refused.
		steps, err := agent.StepsFor(ctx, "skill "+s.Name+" pipeline")
		if err != nil {
			return "", err
		}

		runner := &pipeline.Runner{
			Tool: func(ctx context.Context, name string, args json.RawMessage) (string, error) {
				runFor, _ := pipeline.ToolTimeout(ctx)
				res, err := steps.Run(ctx, name, args, runFor)
				if err != nil {
					return "", err
				}
				if res.IsError {
					return "", fmt.Errorf("%s", res.Content)
				}
				return res.Content, nil
			},
			Model: func(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
				return completeOnce(ctx, adapter, steps.Redact(prompt), schema)
			},
			// Each stage is recorded, so the decomposition, the sufficiency
			// verdict and the reason for every extra hop are visible in the
			// transcript rather than being inferred from tool calls.
			Event: func(stage, detail string, data map[string]any) {
				steps.Stage(s.Name, stage, detail, data)
			},
			// A step's timeout starts once it is approved, not while a person decides.
			ToolTimes: true,
		}

		res, err := runner.Run(ctx, p, input)
		if err != nil {
			return "", err
		}
		return renderForModel(s, res), nil
	}
}

// refuseUnsafe refuses a pipeline whose steps would call a skill: a pipeline
// that runs itself again has no bound.
func refuseUnsafe(p pipeline.Pipeline) error {
	for _, st := range p.Stages {
		for _, step := range st.Steps {
			if step.Kind == "tool" && step.Tool == "skill" {
				return fmt.Errorf("stage %q calls the skill tool; a pipeline step cannot run a skill, so the pipeline was not run", st.Name)
			}
		}
	}
	return nil
}

// completeOnce asks the model for one answer, with no tools offered.
//
// A pipeline's model steps classify, reformulate and judge; giving them the
// tool set would invite them to call something instead of answering, which is
// the failure the pipeline exists to remove.
func completeOnce(ctx context.Context, adapter model.Adapter, prompt string, schema json.RawMessage) (string, error) {
	system := "You answer exactly what is asked, with no preamble."
	if len(schema) > 0 {
		system += " Reply with JSON matching this schema and nothing else:\n" + string(schema)
	}
	stream, err := adapter.Complete(ctx, model.Request{
		System:    system,
		Messages:  []model.Message{{Role: model.RoleUser, Content: prompt}},
		MaxTokens: 2048,
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for chunk := range stream {
		switch chunk.Type {
		case model.ChunkText:
			out.WriteString(chunk.Text)
		case model.ChunkError:
			return "", chunk.Err
		}
	}
	if strings.TrimSpace(out.String()) == "" {
		return "", fmt.Errorf("the model returned nothing")
	}
	return out.String(), nil
}

// renderForModel turns what the pipeline gathered into the context the model
// writes its answer from.
//
// The skill body still comes along: the pipeline guarantees the gathering
// happened, and the instructions say how to present it — citation rules, the
// mode to write in, what never to mention. Those are judgements, which is what
// the model is for.
func renderForModel(s *skills.Skill, res *pipeline.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The %s pipeline ran and gathered the following.\n\n", s.Name)

	for key, val := range res.State.All() {
		if key == "input" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n%s\n\n", key, format(val))
	}
	if len(res.Failures) > 0 {
		b.WriteString("## steps that did not complete\n")
		for _, f := range res.Failures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\nSay so if this changes what you can answer.\n\n")
	}
	b.WriteString("---\n\nNow follow these instructions to write the answer. " +
		"The gathering above has already happened; do not repeat it.\n\n")
	b.WriteString(s.Body)
	return b.String()
}

func format(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

// lastPrompt returns the request the current turn is answering.
//
// A pipeline needs it and the skill tool's arguments do not carry it: the model
// calls the skill by name, not by repeating the question. Holding it here keeps
// the tool's schema unchanged, so a skill invocation still looks the same to
// the model.
var currentPrompt struct {
	mu sync.Mutex
	s  string
}

func setPrompt(p string) {
	currentPrompt.mu.Lock()
	currentPrompt.s = p
	currentPrompt.mu.Unlock()
}

func lastPrompt() string {
	currentPrompt.mu.Lock()
	defer currentPrompt.mu.Unlock()
	return currentPrompt.s
}
