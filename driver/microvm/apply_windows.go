package microvm

import (
	"errors"

	"github.com/zybuu-ai/abhed/driver/microvm/guest"
)

type applier struct{ refused []string }

func openApplier(string, *Credential, []string, int64) (*applier, error) {
	return nil, errors.New("the microvm driver does not run on Windows")
}

func (a *applier) apply(guest.Change) error { return nil }
func (a *applier) close()                   {}
