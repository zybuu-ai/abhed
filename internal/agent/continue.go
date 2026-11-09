package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/model"
)

// EvTurnsExtended is a run that stopped at its turn limit continued by the
// person, with the turns it was given; see TurnsExtended.
const EvTurnsExtended EventType = "turns.extended"

// TurnsExtended is the payload of turns.extended.
type TurnsExtended struct {
	// From and To are the turn limit before and after.
	From int `json:"from"`
	To   int `json:"to"`
	// Count is this continuation's number in the conversation; Max the bound.
	Count int `json:"count"`
	Max   int `json:"max"`
	// By is user: only a person continues a run.
	By string `json:"by"`
	// Surface is cli, tui or print.
	Surface string `json:"surface,omitempty"`
}

// ErrNothingToContinue is ContinueAfterLimit for a conversation whose last
// run did not stop at its turn limit.
var ErrNothingToContinue = errors.New("the last run did not stop at its turn limit, so there is nothing to continue")

// ErrContinueRefused is ContinueAfterLimit past its bound, or where
// continuing is off; the error says which.
var ErrContinueRefused = errors.New("continuing is refused")

// ContinuesLeft is how many more times this conversation may be continued.
func (l *Loop) ContinuesLeft() int { return max(l.Config.MaxContinues-l.continues, 0) }

// NoteContinues counts continuations a resumed conversation already had,
// from its record, so the bound holds across processes.
func (l *Loop) NoteContinues(events []Event) {
	n := 0
	for _, e := range events {
		if e.Type == EvTurnsExtended {
			n++
		}
	}
	l.continues = max(l.continues, n)
	if len(events) > 0 && l.lastReason == "" {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type == EvSessionEnded {
				var end SessionEnded
				if json.Unmarshal(events[i].Payload, &end) == nil {
					l.lastReason = end.Reason
				}
				break
			}
		}
	}
}

// ContinueAfterLimit goes on with a run that stopped at its turn limit: the
// limit moves up by ContinueTurns, the continuation is recorded first, and
// the run resumes where it stopped. It is bounded by MaxContinues for the
// conversation, and refused where that is zero.
func (l *Loop) ContinueAfterLimit(ctx context.Context, surface string) (TerminalReason, error) {
	l.StopSuggestion()
	l.runMu.Lock()
	defer l.unlockRun()
	if l.lastReason != TermMaxTurns {
		return TermError, ErrNothingToContinue
	}
	if l.Config.MaxContinues <= 0 {
		return TermError, fmt.Errorf("%w: the turn limit binds the whole conversation", ErrContinueRefused)
	}
	if l.continues >= l.Config.MaxContinues {
		return TermError, fmt.Errorf("%w: this conversation has been continued %d times, the most limits.max_continues allows", ErrContinueRefused, l.continues)
	}
	n := l.Config.ContinueTurns
	if n <= 0 {
		n = l.Config.TurnsPerMessage
	}
	if n <= 0 {
		n = 25
	}
	ext := TurnsExtended{From: l.Config.MaxTurns, To: l.turns + n, Count: l.continues + 1, Max: l.Config.MaxContinues,
		By: "user", Surface: surface}
	if _, err := l.Recorder.Record(EvTurnsExtended, ActorUser, Trusted, ext); err != nil {
		return TermError, fmt.Errorf("not continued: the continuation could not be recorded: %w", err)
	}
	l.continues++
	l.Config.MaxTurns = ext.To
	l.lastReason = ""
	// The model is told plainly to go on, as the person asked.
	const text = "Continue where you stopped."
	if _, err := l.Recorder.Record(EvUserMessage, ActorUser, Trusted, Message{Text: text}); err != nil {
		return TermError, err
	}
	l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: text})
	return l.run(ctx)
}
