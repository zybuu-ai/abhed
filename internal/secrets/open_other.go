//go:build !unix

package secrets

import "os"

// openStore opens the store; the checks that follow are on the open file.
func openStore(path string) (*os.File, error) { return os.Open(path) }
