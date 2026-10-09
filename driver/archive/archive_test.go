package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLink(t *testing.T) {
	for target, ok := range map[string]bool{
		"b": true, "sub/../b": true, "../x": true, "../../x": false, "/etc": false, "": false,
		".abhed": false, "sub/../.ABHED/config.json": false, "a/.abhed/x": false,
	} {
		if err := Link("a/l", target); (err == nil) != ok {
			t.Errorf("Link(a/l, %q): %v", target, err)
		}
	}
}

// A link into Abhed's state is left out of a pack, and counted.
func TestPackLeavesOutLinksIntoState(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(".abhed/config.json", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	st, err := Pack(context.Background(), dir, &buf, Limits{}, nil)
	if err != nil || st.Entries != 1 || st.Skipped != 1 {
		t.Fatalf("pack: %+v %v", st, err)
	}
}
