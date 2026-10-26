package sandbox

import (
	"context"
	"slices"
	"testing"
)

// A command given the engine's own read-only paths mounts them read-only at
// the same path; one not given them does not.
func TestWithReadOnlyPathsMountsThem(t *testing.T) {
	ws := t.TempDir()
	p := Policy{Workspace: ws}
	plain, err := mountArgs(context.Background(), p, "container", &gitMemory{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithReadOnlyPaths(context.Background(), "/opt/abhed/abhed-linux-arm64")
	with, err := mountArgs(ctx, p, "container", &gitMemory{})
	if err != nil {
		t.Fatal(err)
	}
	want := "/opt/abhed/abhed-linux-arm64:/opt/abhed/abhed-linux-arm64:ro"
	if slices.Contains(plain, want) || !slices.Contains(with, want) {
		t.Fatalf("plain %v\nwith %v", plain, with)
	}
}
