package machinegrant

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// TestMain keeps the tests off this machine's own home and state.
func TestMain(m *testing.M) { testenv.Main(m) }
