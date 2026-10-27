package secretfiles

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A bearer token is seen and hidden; fixtures and prose are neither.
func TestBearerShape(t *testing.T) {
	for _, tok := range []string{
		"Authorization: Bearer 9f8e7d6c5b4a3f2e1d0c",
		`curl -H "Authorization: Bearer AbCdEfGhIjKlMnOpQrStUvWxYzAbCdEfGhIj"`, // letters only, long
		"bearer eyQ1x2Z3k4L5m6N7",
		"Bearer a1b2c3d4e5f6g7h8i9j0",
	} {
		if !HasKeyMaterial(tok) {
			t.Errorf("not seen: %q", tok)
		}
		if out, found := RedactContent(tok); !found || !strings.Contains(out, "[secret]") {
			t.Errorf("not hidden: %q -> %q", tok, out)
		}
	}
	for _, prose := range []string{
		"Bearer test-token-123", `"Bearer abc12345"`, "Bearer fake123token", "Bearer YOUR_TOKEN_1", "Bearer xxxxxxxx1",
		"ring-bearer 2024-01-01", "Bearer RFC6750-compliant", "uses Bearer authentication", "Bearer $TOKEN",
		"Bearer example-token-0123456789abcdef", "Bearer mocktoken1234567890ABCD",
	} {
		if HasKeyMaterial(prose) {
			t.Errorf("taken for a token: %q", prose)
		}
	}
}

// Basic credentials are hidden but do not count as a credential seen.
func TestBasicShapeRedactOnly(t *testing.T) {
	cred := base64.StdEncoding.EncodeToString([]byte("deploy:Opaque-Pass-77"))
	line := "Authorization: Basic " + cred
	if out, found := RedactContent(line); !found || strings.Contains(out, cred) {
		t.Fatalf("Basic not hidden: %q", out)
	}
	if HasKeyMaterial(line) {
		t.Fatal("Basic taken as a credential seen")
	}
	if out, _ := RedactContent("Basic usage of the API is documented"); strings.Contains(out, "[secret]") {
		t.Fatalf("prose hidden: %q", out)
	}
}
