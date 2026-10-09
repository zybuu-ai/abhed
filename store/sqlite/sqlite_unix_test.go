//go:build unix

package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// File modes, links and owners are checked where the system has them.

func TestCreatesPrivateFile(t *testing.T) {
	path := dbPath(t)
	s := open(t, path)
	if _, err := s.Create(context.Background(), newSandbox(t, "alice")); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600, path + "-wal": 0o600} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s is %04o, want %04o", p, info.Mode().Perm(), want)
		}
	}
	var mode string
	if err := s.r.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q %v", mode, err)
	}
	var fk, busy int
	if err := s.w.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys %d %v", fk, err)
	}
	if err := s.w.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != int(busyTimeout.Milliseconds()) {
		t.Fatalf("busy_timeout %d %v", busy, err)
	}
	// The read pool cannot write.
	if _, err := s.r.Exec(`DELETE FROM sandboxes`); err == nil {
		t.Fatal("a read connection wrote")
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, "relative.db"); err == nil {
		t.Error("a relative path was opened")
	}
	if _, err := Open(ctx, filepath.Join(t.TempDir(), "a?b.db")); err == nil {
		t.Error("a path with '?' was opened")
	}

	dir := privateTemp(t)
	real := filepath.Join(dir, "real.db")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, link); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Errorf("a link was opened: %v", err)
	}
	dangling := filepath.Join(dir, "dangling.db")
	if err := os.Symlink(filepath.Join(dir, "nowhere.db"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dangling); err == nil {
		t.Error("a dangling link was opened")
	}
	if _, err := os.Lstat(filepath.Join(dir, "nowhere.db")); !os.IsNotExist(err) {
		t.Errorf("opening through a dangling link created its target: %v", err)
	}

	second := filepath.Join(dir, "second.db")
	if err := os.Link(real, second); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, real); err == nil || !strings.Contains(err.Error(), "names") {
		t.Errorf("a hard-linked file was opened: %v", err)
	}
	_ = os.Remove(second)

	for _, mode := range []os.FileMode{0o620, 0o602, 0o640, 0o666} {
		p := filepath.Join(dir, fmt.Sprintf("mode%o.db", mode))
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, p); err == nil || !strings.Contains(err.Error(), "open to other users") {
			t.Errorf("a file at %04o was opened: %v", mode, err)
		}
		if info, _ := os.Stat(p); info.Mode().Perm() != mode {
			t.Errorf("the refused file was changed to %04o", info.Mode().Perm())
		}
	}

	// A file it cannot read is refused as well.
	if err := os.WriteFile(filepath.Join(dir, "junk.db"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, filepath.Join(dir, "junk.db")); err == nil {
		t.Error("a file that is not a database was opened")
	}

	wal := filepath.Join(dir, "sidecar.db")
	if err := os.WriteFile(wal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, wal+"-wal"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, wal); err == nil || !strings.Contains(err.Error(), "is a link") {
		t.Errorf("a linked -wal file was accepted: %v", err)
	}

	open := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, filepath.Join(open, DefaultName)); err == nil || !strings.Contains(err.Error(), "open to other users") {
		t.Errorf("a folder at 0755 was used: %v", err)
	}
	if _, err := os.Stat(filepath.Join(open, DefaultName)); !os.IsNotExist(err) {
		t.Errorf("a file was created in the refused folder: %v", err)
	}
}
