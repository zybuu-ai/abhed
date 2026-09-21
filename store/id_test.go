package store

import "testing"

// The id is the capability to answer an approval, so it must be unguessable
// and never repeat.
func TestRandomIDIsUniqueAndLongEnough(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id, err := randomID()
		if err != nil {
			t.Fatalf("randomID: %v", err)
		}
		if len(id) != 32 {
			t.Fatalf("id %q is %d hex chars, want 32 (128 bits)", id, len(id))
		}
		if seen[id] {
			t.Fatalf("randomID repeated %q within 1000 draws", id)
		}
		seen[id] = true
	}
}

// An empty id must never reach the database: it would be a shared, guessable
// approval id that any caller could answer.
func TestRandomIDNeverReturnsEmptyWithoutError(t *testing.T) {
	id, err := randomID()
	if err == nil && id == "" {
		t.Fatal("randomID returned an empty id and no error")
	}
}

// testID gives each run its own session id. Fixed ids leave rows behind that
// the next run against the same database collides with.
func testID(t *testing.T, prefix string) string {
	t.Helper()
	r, err := randomID()
	if err != nil {
		t.Fatalf("test id: %v", err)
	}
	return prefix + r
}
