package redteam

import (
	"os"
	"testing"

	"github.com/zybuu-ai/abhed/internal/clitest"
)

// clitest.Main isolates the tests through testenv and removes the binary.
func TestMain(m *testing.M) { os.Exit(clitest.Main(m)) }
