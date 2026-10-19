package policy

import (
	"os"
	"strings"
	"testing"
)

// Every program a wildcard entry may name is in the operators' list in the docs.
func TestKnownReadsAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/ops/infrastructure.md")
	if err != nil {
		t.Fatal(err)
	}
	for name := range knownReads {
		if !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("%s is on the read-only list but not in docs/ops/infrastructure.md", name)
		}
	}
}
