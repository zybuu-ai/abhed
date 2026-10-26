// Command abhed-dapbridge carries a debug adapter's unix socket over stdio
// inside a session's sandbox. Built for linux/amd64 and linux/arm64 with CGO
// off, and shipped beside the engine.
package main

import (
	"os"

	"github.com/zybuu-ai/abhed/internal/dapbridge"
)

func main() { os.Exit(dapbridge.Main(os.Args[1:])) }
