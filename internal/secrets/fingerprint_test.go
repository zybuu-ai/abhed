package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The fingerprint is stable within the process, differs between values, and
// is not the value's plain sha256 or any part of it.
func TestFingerprintIsKeyed(t *testing.T) {
	a, b := Fingerprint([]byte("hunter2")), Fingerprint([]byte("hunter3"))
	if a != Fingerprint([]byte("hunter2")) || a == b || len(a) != 12 {
		t.Fatalf("fingerprints %q %q", a, b)
	}
	sum := sha256.Sum256([]byte("hunter2"))
	if strings.Contains(hex.EncodeToString(sum[:]), a) {
		t.Fatal("the fingerprint is part of the plain sha256")
	}
	if w := Withheld([]byte("hunter2")); !strings.Contains(w, "7 bytes, fingerprint "+a) || strings.Contains(w, "hunter2") {
		t.Fatalf("withheld %q", w)
	}
}
