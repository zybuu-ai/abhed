//go:build !unix

package hostgit

// untrustedFile checks nothing more where links and owners are not known.
func untrustedFile(string) string { return "" }
