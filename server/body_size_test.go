package server

import (
	"net/http"
	"strings"
	"testing"
)

// A body over the cap is refused as too large, not as malformed JSON.
func TestOversizedBodyIs413(t *testing.T) {
	b := newBGServer(t, nil)
	big := `{"prompt":"` + strings.Repeat("a", maxJSONBody+10) + `"}`
	for _, path := range []string{"/v1/sessions", "/v1/sessions/s-x/messages", "/v1/sessions/s-x/approve"} {
		if rec := b.do("alice", "POST", path, big); rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s oversized: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := b.do("alice", "POST", "/v1/sessions", `{"prompt":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: %d %s", rec.Code, rec.Body)
	}
}
