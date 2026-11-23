//go:build !darwin

package modelkeys

// systemKeychain is none: elsewhere keys go in Abhed's own secret store.
func systemKeychain() Keychain { return nil }
