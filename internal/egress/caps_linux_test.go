package egress

import (
	"strings"
	"testing"
)

// A cgo build cannot drop the relay's capabilities on every thread; it says
// so, and how to build one that can, rather than "operation not supported".
func TestDropCapsNamesACgoBuild(t *testing.T) {
	if !cgoBuild {
		t.Skip("built without cgo")
	}
	err := dropCaps(false)
	if err == nil || !strings.Contains(err.Error(), "CGO_ENABLED=0") {
		t.Fatalf("dropCaps in a cgo build: %v", err)
	}
}
