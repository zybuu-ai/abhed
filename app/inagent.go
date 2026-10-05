package app

// InAgentCommand says why this process runs inside an Abhed agent's command,
// or "", so an edition's own subcommand that changes state can refuse it too.
func InAgentCommand() string { return inAgentCommand() }
