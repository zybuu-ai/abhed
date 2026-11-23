package modelkeys

import (
	"os"
	"time"
)

// securityProgram is macOS's own; never one found on PATH.
const securityProgram = "/usr/bin/security"

// systemKeychain is the login Keychain, through the security program.
func systemKeychain() Keychain {
	if _, err := os.Stat(securityProgram); err != nil {
		return nil
	}
	// A lookup waits at most this long, so a locked Keychain's unlock prompt
	// holds a session's start only briefly; the key is then reported missing.
	return Security{Program: securityProgram, Timeout: 20 * time.Second}
}
