package modelkeys

import "os"

// securityProgram is macOS's own; never one found on PATH.
const securityProgram = "/usr/bin/security"

// systemKeychain is the login Keychain, through the security program.
func systemKeychain() Keychain {
	if _, err := os.Stat(securityProgram); err != nil {
		return nil
	}
	return Security{Program: securityProgram}
}
