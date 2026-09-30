package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
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

// isCommandLine reports whether a typed line is for the CLI rather than a
// message: a slash command, a ! shell line or a # memory note. A mid-turn
// line of this kind is held until the turn ends.
func isCommandLine(line string) bool {
	return strings.HasPrefix(line, "/") || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "#")
}

// dispatchLine runs a command line and reports whether the session should end.
func dispatchLine(ctx context.Context, line string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState) bool {
	if strings.HasPrefix(line, "!") {
		runBang(ctx, st, r, strings.TrimSpace(line[1:]))
		return false
	}
	if strings.HasPrefix(line, "#") {
		addNote(ctx, st, r, strings.TrimSpace(line[1:]))
		return false
	}
	return handleCommand(ctx, line, r, pol, sess, st)
}

// ensureConversation opens the session's conversation as a first message
// does, so a line typed before any message has a record to join. A resumed
// session is claimed first, and a pending model switch recorded.
func ensureConversation(ctx context.Context, st *cliState) error {
	if err := claimResumed(ctx, st); err != nil {
		return err
	}
	if err := recordMove(st); err != nil {
		return err
	}
	if st.loop == nil {
		id := newConversationID()
		// The session state's config: /model changes which provider it names.
		if err := recordSession(ctx, st.store, id, st.appCfg); err != nil {
			return err
		}
		st.open(id)
	}
	return nil
}

// expandForTurn is the message a typed line sends, with its @ mentions
// attached through the session's policy. ok is false when the line must not
// be sent; why has been shown.
func expandForTurn(ctx context.Context, st *cliState, r *ui.Renderer, line string) (agent.Message, bool) {
	msg, _, err := (mentionExpander{st: st}).Expand(ctx, st.loop, line)
	if err != nil {
		surfaceOf(st, r).Append(ui.Block{Kind: ui.BlockError, Text: "not sent: " + err.Error()})
		return agent.Message{}, false
	}
	return msg, true
}

// surfaceOf is the session's surface, or a line surface that answers
// nothing, so a question asked before the terminal UI is ready is refused.
func surfaceOf(st *cliState, r *ui.Renderer) ui.Surface {
	if st.surface != nil {
		return st.surface
	}
	s := ui.Style{}
	if r != nil {
		s = r.Style()
	}
	return ui.NewLineSurface(ui.LazyStdout{}, s, nil)
}

// redactFor runs the conversation's secret redactor over text, as the record
// would. It fails closed: text that cannot be redacted is withheld.
func redactFor(loop *agent.Loop, text string) string {
	if loop == nil || loop.Recorder == nil || loop.Recorder.Redact == nil {
		return text
	}
	red := loop.Recorder.Redact
	if v := reflect.ValueOf(red); v.Kind() == reflect.Pointer && v.IsNil() {
		return text
	}
	raw, err := json.Marshal(text)
	if err != nil {
		return agent.Withheld
	}
	var out string
	if json.Unmarshal(red.Redact(raw), &out) != nil {
		return agent.Withheld
	}
	return out
}

// personCallID is an id for a call the person makes from the prompt.
func personCallID(kind string) string {
	return kind + "-" + strings.TrimPrefix(newConversationID(), "s-")
}

// argsJSON marshals a tool call's arguments.
func argsJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("abhed: marshal arguments: %v", err))
	}
	return b
}
