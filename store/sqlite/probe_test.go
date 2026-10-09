//go:build unix

package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// listing names every file under dir with its size, to show nothing changed.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprint(p, " ", info.Mode(), " ", info.ModTime().UnixNano(), " ", info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInspectChangesNothing(t *testing.T) {
	ctx := context.Background()
	base := privateTemp(t)

	// No folder yet: reported, not made.
	path := filepath.Join(base, "cp", DefaultName)
	p, err := Inspect(ctx, path)
	if err != nil || p.Exists || p.String() != "would be created at "+path {
		t.Fatalf("missing folder: %+v %v", p, err)
	}
	if _, err := os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("the folder was created: %v", err)
	}

	// A folder but no file.
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := Inspect(ctx, path); err != nil || p.Exists {
		t.Fatalf("missing file: %+v %v", p, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the file was created: %v", err)
	}

	// An empty file is a database at version 0: a migration is due, not applied.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before := listing(t, base)
	p, err = Inspect(ctx, path)
	if err != nil || !p.Exists || p.Version != 0 || !strings.Contains(p.String(), "serve will migrate it to") {
		t.Fatalf("old schema: %+v %v", p, err)
	}
	if after := listing(t, base); !slices.Equal(before, after) {
		t.Fatalf("inspecting changed the folder:\n%v\n%v", before, after)
	}

	// A current file reports its version and is left as it was.
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before = listing(t, base)
	p, err = Inspect(ctx, path)
	if err != nil || !p.Exists || p.Version != SchemaVersion || strings.Contains(p.String(), "migrate") {
		t.Fatalf("current schema: %+v %v", p, err)
	}
	if after := listing(t, base); !slices.Equal(before, after) {
		t.Fatalf("inspecting changed the folder:\n%v\n%v", before, after)
	}

	// While a server holds it open, its log is read too.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := Inspect(ctx, path); err != nil || p.Version != SchemaVersion {
		t.Fatalf("while open: %+v %v", p, err)
	}

	// A newer file is refused, as Open refuses it.
	if _, err := s.w.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (?, 'later')`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Inspect(ctx, path); err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("a newer schema passed: %v", err)
	}
}

func TestInspectRefuses(t *testing.T) {
	ctx := context.Background()
	if _, err := Inspect(ctx, "relative.db"); err == nil {
		t.Error("a relative path passed")
	}
	open := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, filepath.Join(open, DefaultName)); err == nil || !strings.Contains(err.Error(), "open to other users") {
		t.Errorf("a folder at 0755 passed: %v", err)
	}
	dir := privateTemp(t)
	real := filepath.Join(dir, "real.db")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "link.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, filepath.Join(dir, "link.db")); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Errorf("a link passed: %v", err)
	}
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, real); err == nil || !strings.Contains(err.Error(), "open to other users") {
		t.Errorf("a file at 0644 passed: %v", err)
	}
}
