// Command abhed is the Community Edition binary. Everything it does lives in
// the app package; this main exists so the linker has somewhere to put the
// version. Its test keeps every package under ee/ out of its dependencies.
package main

import (
	"os"
	"runtime/debug"

	"github.com/zybuu-ai/abhed/app"
)

// version is set by the release build (-ldflags "-X main.version=v0.1.0").
// A `go install .../cmd/abhed@v0.1.0` build has no ldflags, but the module
// system records the version it resolved, so the binary reads that instead
// of reporting itself as a development build it is not.
var version = "0.1.0-dev"

func main() {
	if version == "0.1.0-dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
	}
	os.Exit(app.Main(os.Args[1:], app.WithVersion(version)))
}
