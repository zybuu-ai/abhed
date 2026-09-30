package local

import "os"

// syncFile makes what was written to f durable.
func syncFile(f *os.File) error { return f.Sync() }
