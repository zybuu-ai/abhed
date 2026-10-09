package storetest_test

import (
	"testing"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/controlplane/storetest"
)

// The suite itself is checked against the reference store.
func TestRunAgainstMemory(t *testing.T) {
	storetest.Run(t, func(*testing.T) controlplane.Store { return controlplane.NewMemory() })
}
