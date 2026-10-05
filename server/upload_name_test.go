package server

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A long upload name is cut to 120 bytes on a character boundary, keeping a
// short extension; one too long to be an extension does not panic the cut.
func TestLongUploadNameIsCutWhole(t *testing.T) {
	for raw, ext := range map[string]string{
		strings.Repeat("é", 100) + ".txt":      ".txt",
		"ab" + strings.Repeat("ü", 70) + ".md": ".md",
		"x." + strings.Repeat("y", 200):        "",
		"x." + strings.Repeat("ж", 100):        "",
	} {
		name := cleanUploadName(raw)
		if len(name) > 120 || !utf8.ValidString(name) || !strings.HasSuffix(name, ext) || name == "" {
			t.Errorf("cleanUploadName(%.20q…) = %q (%d bytes)", raw, name, len(name))
		}
	}
}
