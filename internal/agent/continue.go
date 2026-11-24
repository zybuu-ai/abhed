package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
// conversation, and refused where that is zero. The message that resumes it
// is the person's, so it goes through what RunMessage does: queued messages
// first, the prompt hooks, and a fresh start for the repeat guards.
func (l *Loop) ContinueAfterLimit(ctx context.Context, surface string) (_ TerminalReason, err error) {
	defer func() { err = l.redactErr(err) }()
	l.StopSuggestion()
	l.runMu.Lock()
	defer l.unlockRun()
	if err := l.canContinue(); err != nil {
		return TermError, err
	}
	// The model is told plainly to go on, as the person asked.
	const text = "Continue where you stopped."
	if _, err := l.deliverQueuedCount(); err != nil {
		return TermError, err
	}
	l.setRefused("")
	if why := l.promptRefused(ctx, text); why != "" {
		l.record(EvMessageDropped, ActorSystem, DroppedMessage{Text: text, QueuedAt: time.Now(), Reason: why})
		l.setRefused(why)
		return TermPromptRefused, nil
	}
	ext := TurnsExtended{From: l.Config.MaxTurns, To: l.turns + l.continueTurns(), Count: l.continues + 1, Max: l.Config.MaxContinues,
		By: "user", Surface: surface}
	if _, err := l.Recorder.Record(EvTurnsExtended, ActorUser, Trusted, ext); err != nil {
		return TermError, fmt.Errorf("not continued: the continuation could not be recorded: %w", err)
	}
	l.continues++
	l.lastReason = ""
	if _, err := l.Recorder.Record(EvUserMessage, ActorUser, Trusted, Message{Text: text}); err != nil {
		return TermError, err
	}
	l.messages = append(l.messages, model.Message{Role: model.RoleUser, Content: text})
	l.noteSaid(text, false)
	l.setPrompt(text, text)
	l.startMessage()
	l.Config.MaxTurns = ext.To
	return l.run(ctx)
}

// ContinueRefusal is why a conversation with these events could not be
// continued under maxContinues, or nil: checked before anything is written
// for it, such as a branch.
func ContinueRefusal(events []Event, maxContinues int) error {
	l := &Loop{Config: Config{MaxContinues: maxContinues}}
	l.NoteContinues(events)
	return l.canContinue()
}

// CanContinue reports why ContinueAfterLimit would refuse now, or nil.
func (l *Loop) CanContinue() error {
	l.runMu.Lock()
	defer l.runMu.Unlock()
	return l.canContinue()
}

func (l *Loop) canContinue() error {
	if l.lastReason != TermMaxTurns {
		return ErrNothingToContinue
	}
	if l.Config.MaxContinues <= 0 {
		return fmt.Errorf("%w: the turn limit binds the whole conversation", ErrContinueRefused)
	}
	if l.continues >= l.Config.MaxContinues {
		return fmt.Errorf("%w: this conversation has been continued %d times, the most limits.max_continues allows", ErrContinueRefused, l.continues)
	}
	return nil
}

// continueTurns is how many turns a continuation gives: ContinueTurns,
// else TurnsPerMessage, else 25.
func (l *Loop) continueTurns() int {
	if n := l.Config.ContinueTurns; n > 0 {
		return n
	}
	if n := l.Config.TurnsPerMessage; n > 0 {
		return n
	}
	return 25
}
