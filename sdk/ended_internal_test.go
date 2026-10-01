package abhed

import (
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A run that ended early says why as a value, with the message it always had.
func TestEndedErrorCarriesTheReason(t *testing.T) {
	var err error = &EndedError{Reason: agent.TermMaxTurns}
	var ended *EndedError
	if !errors.As(err, &ended) || ended.Reason != agent.TermMaxTurns || err.Error() != "abhed: ended as max_turns" {
		t.Fatalf("%v", err)
	}
}
