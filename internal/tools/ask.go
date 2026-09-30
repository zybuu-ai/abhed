package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Ask lets the agent put a multiple-choice question to the person, when it
// cannot go on well without their choice. It is not an approval: the answer
// grants nothing, changes no rule or mode, and is never given on the
// person's behalf. With no one to ask, the tool says so and the agent decides.
type Ask struct {
	mu    sync.RWMutex
	asker Asker
}

// Asker puts a question to the person and returns the chosen option's
// label, or ErrNotAnswered.
type Asker interface {
	AskPerson(ctx context.Context, q Question) (string, error)
}

// ErrNotAnswered is a question the person did not answer.
var ErrNotAnswered = errors.New("not answered")

// Question is what the agent asks.
type Question struct {
	Question string   `json:"question"`
	Options  []Option `json:"options"`
}

// Option is one answer the agent offers.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Bounds on a question.
const (
	askMinOptions  = 2
	askMaxOptions  = 6
	askMaxQuestion = 500
	askMaxLabel    = 120
	askMaxDesc     = 300
)

// NewAsk is an ask tool with no one to ask yet.
func NewAsk() *Ask { return &Ask{} }

// SetAsker sets who answers; nil leaves no one.
func (a *Ask) SetAsker(x Asker) {
	a.mu.Lock()
	a.asker = x
	a.mu.Unlock()
}

func (*Ask) Name() string  { return "ask_user" }
func (*Ask) Mutates() bool { return false }

func (*Ask) Description() string {
	return "Ask the person a multiple-choice question when a decision is theirs and you cannot go on well without it " +
		"(which approach, which of two readings of the request). Offer 2-6 short options. " +
		"It is not a way to get permission: tool calls are approved separately. Do not ask what you can find out yourself."
}

func (*Ask) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "question":{"type":"string","description":"The question, one or two sentences."},
    "options":{"type":"array","minItems":2,"maxItems":6,"items":{"type":"object","properties":{
      "label":{"type":"string","description":"A short answer."},
      "description":{"type":"string","description":"What choosing it means."}},"required":["label"]}}
  },
  "required":["question","options"]
}`)
}

func (a *Ask) Run(ctx context.Context, _ *Session, raw json.RawMessage) Result {
	var q Question
	if err := json.Unmarshal(raw, &q); err != nil {
		return errf("Invalid arguments for ask_user: %v", err)
	}
	q.Question = strings.TrimSpace(q.Question)
	switch {
	case q.Question == "":
		return errf("question is required.")
	case len(q.Question) > askMaxQuestion:
		return errf("The question is longer than %d characters; ask it shorter.", askMaxQuestion)
	case len(q.Options) < askMinOptions || len(q.Options) > askMaxOptions:
		return errf("Offer between %d and %d options.", askMinOptions, askMaxOptions)
	}
	seen := map[string]bool{}
	for i, o := range q.Options {
		l := strings.TrimSpace(o.Label)
		if l == "" || len(l) > askMaxLabel || seen[strings.ToLower(l)] {
			return errf("Option %d needs a short label of its own.", i+1)
		}
		if len(o.Description) > askMaxDesc {
			return errf("Option %d's description is longer than %d characters.", i+1, askMaxDesc)
		}
		seen[strings.ToLower(l)] = true
		q.Options[i].Label = l
	}
	a.mu.RLock()
	asker := a.asker
	a.mu.RUnlock()
	if asker == nil {
		return ok("No one is at the prompt to answer. Decide yourself, and say what you chose and why.")
	}
	answer, err := asker.AskPerson(ctx, q)
	if errors.Is(err, ErrNotAnswered) {
		return ok("The person did not answer. Do not assume an answer: say what you need, or take the safest reading and say so.")
	}
	if err != nil {
		return errf("The question could not be asked: %v", err)
	}
	return ok("The person chose: %s", answer)
}

// String describes the question for a log line.
func (q Question) String() string {
	return fmt.Sprintf("%s (%d options)", q.Question, len(q.Options))
}
