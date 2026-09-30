package app

import (
	"context"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// InputExpander turns a submitted line into the message the model gets:
// @ mentions attached through policy, ! output joined, # notes written. The
// turn driver calls it on submit; a custom command calls it on its body.
type InputExpander interface {
	// Expand returns the message to send and what it attached. loop is the
	// conversation, whose session and policy the reads go through; it may be
	// nil before the first turn.
	Expand(ctx context.Context, loop *agent.Loop, raw string) (agent.Message, []Attachment, error)
}

// Attachment is one file an expanded message carries.
type Attachment struct {
	Path string // relative to the workspace
	// Range is the lines attached, as "10-20"; "" for the whole file.
	Range     string
	SHA256    string
	Bytes     int64
	Truncated bool
}

// Mention is what input.mention records for the attachment.
func (a Attachment) Mention() agent.InputMention {
	return agent.InputMention{Path: a.Path, Range: a.Range, SHA256: a.SHA256, Bytes: a.Bytes, Truncated: a.Truncated}
}

// plainInput sends the line as typed: nothing is expanded or attached.
type plainInput struct{}

var _ InputExpander = plainInput{}

func (plainInput) Expand(_ context.Context, _ *agent.Loop, raw string) (agent.Message, []Attachment, error) {
	return agent.Message{Text: raw}, nil, nil
}
