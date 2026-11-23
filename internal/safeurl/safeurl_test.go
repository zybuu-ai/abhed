package safeurl

import (
	"strings"
	"testing"
)

const secretURL = "https://orguser:SECRET-pw@127.0.0.1:9/pol/path?token=SECRET-q&x=1#SECRET-frag"

func TestServerKeepsSchemeHostPath(t *testing.T) {
	if got := Server(secretURL); got != "https://127.0.0.1:9/pol/path" {
		t.Fatalf("Server = %q", got)
	}
	for _, bad := range []string{"https://SECRET@", "::SECRET", "SECRET-no-scheme"} {
		if got := Server(bad); strings.Contains(got, "SECRET") {
			t.Errorf("Server(%q) = %q", bad, got)
		}
	}
	if Server("") != "" {
		t.Error("an empty URL is not named")
	}
}

func TestRedactedHidesEverySecretPart(t *testing.T) {
	got := Redacted(secretURL)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "orguser") {
		t.Fatalf("Redacted = %q", got)
	}
	for _, want := range []string{"https://redacted@127.0.0.1:9/pol/path?", "token=redacted", "x=redacted", "#redacted"} {
		if !strings.Contains(got, want) {
			t.Errorf("Redacted = %q, missing %q", got, want)
		}
	}
	if got := Redacted("https://ghp_SECRET@example.com/p"); strings.Contains(got, "SECRET") {
		t.Fatalf("a token as the user part: %q", got)
	}
}
