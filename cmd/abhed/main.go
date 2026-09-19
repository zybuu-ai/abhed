// Command abhed is the Community Edition binary. Everything it does lives in
// the app package; this main exists so the linker has somewhere to put the
// version. Its test keeps every package under ee/ out of its dependencies.
package main

import (
	"os"
	"runtime/debug"

	"github.com/zybuu-ai/abhed/app"
)

// devVersion is what an unstamped build reports. Compared against rather than
// repeated as a literal: the same string appeared twice and a release bump
// silently broke the check.
const devVersion = "dev"

// version is set by the release build (-ldflags "-X main.version=v0.2.0").
// A `go install .../cmd/abhed@v0.2.0` build has no ldflags, but the module
// system records the version it resolved, so the binary reads that instead
// of reporting itself as a development build it is not.
var version = devVersion

func main() {
	if version == devVersion {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
	}
	os.Exit(app.Main(os.Args[1:], app.WithVersion(version)))
}
