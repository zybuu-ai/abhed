package metrics

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// The test binary links the managed paths; keep its tests off this
// machine's managed files and model services.
func TestMain(m *testing.M) { testenv.Main(m) }
