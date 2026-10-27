//go:build !unix

package app

// folderID has no identity to give here; grants follow the path alone.
func folderID(string) string { return "" }
