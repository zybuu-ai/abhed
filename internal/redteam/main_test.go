package redteam

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/mcp/mcptest"
	"github.com/zybuu-ai/abhed/internal/testenv"
)

// The test binary links the managed paths; keep its tests off this
// machine's managed files and model services.
// Started with mcptest's marker, the binary is a stub MCP server instead.
func TestMain(m *testing.M) {
	if mcptest.Serve() {
		return
	}
	testenv.Main(m)
}
