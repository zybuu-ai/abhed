package toolset

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/mcp/mcptest"
	"github.com/zybuu-ai/abhed/internal/testenv"
)

// TestMain keeps the tests off this machine's own configuration, managed
// files and model services (internal/testenv); a test that needs one sets its own.
// Started with mcptest's marker, the binary is a stub MCP server instead.
func TestMain(m *testing.M) {
	if mcptest.Serve() {
		return
	}
	testenv.Main(m)
}
