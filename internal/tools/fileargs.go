package tools

import "encoding/json"

// FileArgs marks a tool whose call reads or writes workspace files beside
// what it does elsewhere, such as a file sent to a host. Each path is judged
// as a read or a write of it would be, so a rule on a file holds whichever
// tool reaches it.
type FileArgs interface {
	FilesRead(args json.RawMessage) []string
	FilesWritten(args json.RawMessage) []string
}
