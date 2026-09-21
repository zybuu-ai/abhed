package tools

import (
	"strings"
	"testing"
)

// Only the first 8000 bytes are inspected, and that cut used to be judged as
// it fell: through the middle of a multi-byte character it is invalid UTF-8,
// so a long file in any script but Latin was refused as binary.
func TestIsBinaryDoesNotMistakeACutCharacterForBinary(t *testing.T) {
	for pad := 0; pad < 3; pad++ {
		text := strings.Repeat("a", 7998+pad) + strings.Repeat("अ", 100)
		if IsBinary([]byte(text)) {
			t.Fatalf("text with a character across byte 8000 (pad %d) was called binary", pad)
		}
	}
	if !IsBinary([]byte("PK\x03\x04\x00\x00")) {
		t.Fatal("a NUL byte must still mean binary")
	}
	if !IsBinary(append([]byte(strings.Repeat("a", 100)), 0xff, 0xfe)) {
		t.Fatal("invalid UTF-8 must still mean binary")
	}
}
