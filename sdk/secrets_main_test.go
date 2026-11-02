package abhed

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// TestMain keeps the tests off this machine's own configuration, managed
// files, secrets and model services (internal/testenv); a test that needs one sets its own.
func TestMain(m *testing.M) { testenv.Main(m) }
