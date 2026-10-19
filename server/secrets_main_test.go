package server

import (
	"os"
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// engineHome is the home the tests started with, where a real container
// engine finds its connection.
var engineHome string

// TestMain keeps the tests off this machine's own configuration, managed
// files, secrets and model services (internal/testenv); a test that needs
// one sets its own.
func TestMain(m *testing.M) {
	g := testenv.Isolate()
	engineHome = g.OrigHome
	os.Exit(g.Close(m.Run()))
}
