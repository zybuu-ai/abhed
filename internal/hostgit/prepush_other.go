//go:build !unix

package hostgit

import (
	"context"
	"errors"
)

// prePushRelay is not made here: with no pipe to hand a hook's input back,
// a push with the person's hooks is refused rather than made without them.
type prePushRelay struct{ dir string }

func (b *HostBroker) prePushRelay(context.Context, plan) (*prePushRelay, error) {
	return nil, errors.New("hooks cannot be run in the sandbox on this system")
}

func (r *prePushRelay) close() (bool, int) { return false, 0 }
