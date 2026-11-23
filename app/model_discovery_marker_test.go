package app

import (
	"testing"

	"github.com/zybuu-ai/abhed/internal/testenv"
)

// The marker the release code reads is the test environment's.
func TestEnvMarkerMatchesTestenv(t *testing.T) {
	if testEnvMarker != testenv.Marker {
		t.Fatalf("%q, want %q", testEnvMarker, testenv.Marker)
	}
}
