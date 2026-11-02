package sandbox

import (
	"context"
	"slices"
	"strings"
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

// On the Linux process tier a command given the engine's read-only paths
// binds them read-only at the same path, and one given private folders gets
// each as a tmpfs only it sees, after the shared /tmp; others get neither.
func TestBwrapReadOnlyAndPrivateDirs(t *testing.T) {
	p := DefaultPolicy(workspace(t))
	p.PrivateTemp = true
	s := &Process{policy: p, backend: "bwrap"}
	s.freshOnce.Do(func() { s.freshOK = true })
	t.Cleanup(func() { _ = s.removeTemp() })
	plain := strings.Join(s.wrapEgress(t.Context(), p.Workspace, nil, nil, "/bin/true").Args, " ")
	ctx := WithPrivateDirs(WithReadOnlyPaths(t.Context(), "/opt/abhed/abhed-linux-arm64"), "/run/abhed-dap-0f1e")
	with := s.wrapEgress(ctx, p.Workspace, nil, nil, "/bin/true").Args
	joined := strings.Join(with, " ")
	for _, want := range []string{"--ro-bind /opt/abhed/abhed-linux-arm64 /opt/abhed/abhed-linux-arm64", "--tmpfs /run/abhed-dap-0f1e"} {
		if !strings.Contains(joined, want) || strings.Contains(plain, want) {
			t.Errorf("%q: with %v\nplain %s", want, with, plain)
		}
	}
	if own, _ := s.privateTemp(); slices.Index(with, "/run/abhed-dap-0f1e") < slices.Index(with, own) {
		t.Errorf("the private folder is mounted before the shared /tmp: %v", with)
	}
}
