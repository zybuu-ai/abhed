//go:build !unix

package app

import "os"

// stdinWaiting reports whether stdin is a non-empty file; a pipe is not
// looked into here.
func stdinWaiting() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}
