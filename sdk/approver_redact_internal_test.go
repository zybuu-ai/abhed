package abhed

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/secrets"
)

// The approver's decision, its suggested scope and reason included, is
// redacted as the arguments are. A write whose path holds a stored value is
// now refused before anyone is asked, so the approver is driven directly.
func TestSDKApproverScopeIsRedacted(t *testing.T) {
	const value = "approver-secret-4b7e"
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(path, []byte(`{"FAKE_TOKEN":"`+value+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := secrets.Open(path)
	first, err := store.LoadRedactor()
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	ap := approverFor(func(_ context.Context, _ string, args json.RawMessage, d Decision) (bool, error) {
		seen = append(seen, d.Scope, d.Reason, string(args))
		return false, nil
	}, store.Fresh(first))
	_, _ = ap.Approve(context.Background(), "write", json.RawMessage(`{"path":"n-`+value+`.txt"}`),
		Decision{Scope: "write(n-" + value + ".txt)", Reason: "writes n-" + value + ".txt"})
	got := strings.Join(seen, "\n")
	if len(seen) != 3 || strings.Count(got, "[secret:FAKE_TOKEN]") != 3 || strings.Contains(got, value) {
		t.Fatalf("the approver's decision was not redacted: %q", got)
	}
}
