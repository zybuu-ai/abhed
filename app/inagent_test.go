package app

import "testing"

// An edition's subcommand sees the same answer the built-in ones refuse on.
func TestInAgentCommandIsTheBuiltInsAnswer(t *testing.T) {
	old := inAgentCommand
	t.Cleanup(func() { inAgentCommand = old })
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	if got := InAgentCommand(); got != "ABHED_SANDBOX is set" {
		t.Errorf("InAgentCommand() = %q", got)
	}
	inAgentCommand = func() string { return "" }
	if got := InAgentCommand(); got != "" {
		t.Errorf("InAgentCommand() = %q outside an agent's command", got)
	}
}
