//go:build !windows

package snapshot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/driver"
)

func dirs(t *testing.T) (store Store, ws, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, outside = filepath.Join(base, "ws"), filepath.Join(base, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return Store{Dir: filepath.Join(base, "snaps")}, ws, outside
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// A round trip puts the workspace back as it was, removes what came after,
// never snapshots or touches .abhed, and leaves links out of the
// workspace out of the archive.
func TestTakeAndRestoreRoundTrip(t *testing.T) {
	s, ws, outside := dirs(t)
	ctx := context.Background()
	write(t, filepath.Join(ws, "a.txt"), "one")
	write(t, filepath.Join(ws, "sub", "b.txt"), "two")
	write(t, filepath.Join(ws, ".abhed", "state.json"), "state-at-snapshot")
	write(t, filepath.Join(ws, "nested", ".ABHED", "x"), "nested state")
	write(t, filepath.Join(outside, "secret"), "host secret")
	if err := os.Symlink("sub/b.txt", filepath.Join(ws, "inside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(ws, "out-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(ws, "a.txt"), 0o4755); err != nil {
		t.Fatal(err)
	}

	ref, st, err := s.Take(ctx, "sbx_1", 3, "before", ws)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(ref.ID) || ref.Generation != 3 || ref.Name != "before" || !strings.HasPrefix(ref.Digest, "sha256:") || ref.Bytes == 0 {
		t.Fatalf("ref %+v", ref)
	}
	if st.Skipped != 1 {
		t.Fatalf("the link out of the workspace should be skipped: %+v", st)
	}
	info, err := os.Stat(filepath.Join(s.Dir, "sbx_1"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the store folder must be private: %v %v", info.Mode(), err)
	}

	// Changes after the snapshot, including to the state.
	write(t, filepath.Join(ws, "a.txt"), "changed")
	write(t, filepath.Join(ws, "new.txt"), "new")
	if err := os.RemoveAll(filepath.Join(ws, "sub")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, ".abhed", "state.json"), "state-now")

	if _, err := s.Restore(ctx, "sbx_1", ref, ws); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"a.txt": "one", "sub/b.txt": "two", "inside-link": "two",
		".abhed/state.json": "state-now", "nested/.ABHED/x": "nested state",
	} {
		if got := read(t, filepath.Join(ws, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(ws, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file made after the snapshot survived the restore: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "out-link")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link out of the workspace came back: %v", err)
	}
	if info, _ := os.Stat(filepath.Join(ws, "a.txt")); info.Mode()&os.ModeSetuid != 0 {
		t.Error("the setuid bit survived")
	}
	entries, _ := os.ReadDir(ws)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingPrefix) {
			t.Errorf("the restore left %s behind", e.Name())
		}
	}
	if got := read(t, filepath.Join(outside, "secret")); got != "host secret" {
		t.Fatalf("outside changed: %q", got)
	}
}

// Content that differs from the ref's digest is refused before the
// workspace is touched, as is a ref with no digest.
func TestRestoreRefusesDigestMismatch(t *testing.T) {
	s, ws, _ := dirs(t)
	ctx := context.Background()
	write(t, filepath.Join(ws, "a.txt"), "one")
	ref, _, err := s.Take(ctx, "sbx_1", 1, "", ws)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws, "a.txt"), "now")

	bad := ref
	bad.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := s.Restore(ctx, "sbx_1", bad, ws); !errors.Is(err, driver.ErrInvalid) || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a wrong digest: %v", err)
	}
	for _, d := range []string{"", "md5:abc", "sha256:xyz"} {
		r := ref
		r.Digest = d
		if _, err := s.Restore(ctx, "sbx_1", r, ws); !errors.Is(err, driver.ErrInvalid) {
			t.Fatalf("digest %q: %v", d, err)
		}
	}

	// The file on disk is changed under a ref that still names the old content.
	file := filepath.Join(s.Dir, "sbx_1", ref.ID+".tar.gz")
	tampered := tarGz(t, []entry{{name: "a.txt", body: "planted"}})
	if err := os.WriteFile(file, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, "sbx_1", ref, ws); !errors.Is(err, driver.ErrInvalid) {
		t.Fatalf("tampered content: %v", err)
	}
	if got := read(t, filepath.Join(ws, "a.txt")); got != "now" {
		t.Fatalf("the workspace changed under a refused restore: %q", got)
	}
}

// Another sandbox's snapshot, and an id that would name a path, are not found.
func TestRestoreScopedToTheSandbox(t *testing.T) {
	s, ws, _ := dirs(t)
	ctx := context.Background()
	write(t, filepath.Join(ws, "a.txt"), "one")
	ref, _, err := s.Take(ctx, "sbx_1", 1, "", ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, "sbx_2", ref, ws); !errors.Is(err, driver.ErrNotFound) {
		t.Fatalf("another sandbox's snapshot: %v", err)
	}
	for _, id := range []string{"../sbx_1/" + ref.ID, "snap_../../x", ""} {
		r := ref
		r.ID = id
		if _, err := s.Restore(ctx, "sbx_1", r, ws); !errors.Is(err, driver.ErrInvalid) {
			t.Fatalf("id %q: %v", id, err)
		}
	}
	for _, sid := range []string{"../x", ".", "a/b", ""} {
		if _, _, err := s.Take(ctx, sid, 1, "", ws); err == nil {
			t.Fatalf("sandbox id %q named a folder", sid)
		}
	}
}

type entry struct {
	name, body, link string
	typ              byte
}

func tarGz(t *testing.T, es []entry) []byte {
	t.Helper()
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ, Linkname: e.link}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if h.Typeflag == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	return raw.Bytes()
}

// plant writes a crafted archive as a snapshot of sbx_1 and returns a ref
// with its true digest, as a ref holder who controlled the content would.
func plant(t *testing.T, s Store, es []entry) driver.SnapshotRef {
	t.Helper()
	b := tarGz(t, es)
	sum := sha256.Sum256(b)
	ref := driver.SnapshotRef{ID: "snap_00112233445566ff", Digest: "sha256:" + hex.EncodeToString(sum[:])}
	dir := filepath.Join(s.Dir, "sbx_1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ref.ID+".tar.gz"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return ref
}

// Even with a matching digest, an archive that would write outside the
// workspace or into Abhed's state is refused, and the workspace is left
// exactly as it was.
func TestRestoreRefusesEscapes(t *testing.T) {
	cases := map[string][]entry{
		"dot-dot":            {{name: "../escaped", body: "x"}},
		"nested dot-dot":     {{name: "a/../../escaped", body: "x"}},
		"absolute":           {{name: "/tmp/escaped", body: "x"}},
		"state":              {{name: ".abhed/config.json", body: "x"}},
		"state any case":     {{name: "sub/.AbHeD/x", body: "x"}},
		"absolute link":      {{name: "l", typ: tar.TypeSymlink, link: "/etc"}},
		"link climbing out":  {{name: "d/l", typ: tar.TypeSymlink, link: "../../outside"}},
		"link then write":    {{name: "l", typ: tar.TypeSymlink, link: "."}, {name: "l/x", body: "x"}},
		"hard link out":      {{name: "h", typ: tar.TypeLink, link: "../outside/secret"}},
		"hard link to state": {{name: "h", typ: tar.TypeLink, link: ".abhed/x"}},
		"device":             {{name: "dev", typ: tar.TypeChar}},
		"fifo":               {{name: "p", typ: tar.TypeFifo}},
		"duplicate":          {{name: "a", body: "1"}, {name: "a", body: "2"}},
		// Two links that each stay inside on paper, chained to climb out.
		"chained links": {
			{name: "a/q", typ: tar.TypeSymlink, link: "../b"},
			{name: "a/p", typ: tar.TypeSymlink, link: "q/../.."},
			{name: "b", typ: tar.TypeDir},
			{name: "a/p/outside/planted", body: "x"},
		},
	}
	for name, es := range cases {
		t.Run(name, func(t *testing.T) {
			s, ws, outside := dirs(t)
			write(t, filepath.Join(ws, "keep.txt"), "kept")
			write(t, filepath.Join(ws, ".abhed", "x"), "state")
			write(t, filepath.Join(outside, "secret"), "host secret")
			ref := plant(t, s, es)
			if _, err := s.Restore(context.Background(), "sbx_1", ref, ws); err == nil {
				t.Fatal("the archive was restored")
			}
			if got := read(t, filepath.Join(ws, "keep.txt")); got != "kept" {
				t.Fatalf("the workspace changed: %q", got)
			}
			if got := read(t, filepath.Join(ws, ".abhed", "x")); got != "state" {
				t.Fatalf("the state changed: %q", got)
			}
			base := filepath.Dir(ws)
			for _, p := range []string{filepath.Join(base, "escaped"), filepath.Join(outside, "planted"), "/tmp/escaped"} {
				if _, err := os.Lstat(p); err == nil && p != "/tmp/escaped" {
					t.Fatalf("%s was written", p)
				}
			}
			if got := read(t, filepath.Join(outside, "secret")); got != "host secret" {
				t.Fatalf("outside changed: %q", got)
			}
			entries, _ := os.ReadDir(ws)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), stagingPrefix) {
					t.Fatalf("a refused restore left %s behind", e.Name())
				}
			}
		})
	}
}

// A snapshot with a file where the workspace has a folder holding nested
// state is refused before anything is removed.
func TestRestoreRefusesClashWithNestedState(t *testing.T) {
	s, ws, _ := dirs(t)
	write(t, filepath.Join(ws, "keep.txt"), "kept")
	write(t, filepath.Join(ws, "proj", ".abhed", "x"), "nested state")
	ref := plant(t, s, []entry{{name: "proj", body: "a file now"}})
	if _, err := s.Restore(context.Background(), "sbx_1", ref, ws); !errors.Is(err, driver.ErrConflict) {
		t.Fatalf("clash: %v", err)
	}
	if read(t, filepath.Join(ws, "keep.txt")) != "kept" || read(t, filepath.Join(ws, "proj", ".abhed", "x")) != "nested state" {
		t.Fatal("the workspace changed under a refused restore")
	}
}

// A store inside the workspace, or holding it, is refused: the sandbox
// could read other snapshots there.
func TestStoreMustBeOutsideTheWorkspace(t *testing.T) {
	_, ws, _ := dirs(t)
	write(t, filepath.Join(ws, "a"), "x")
	for _, dir := range []string{filepath.Join(ws, "snaps"), filepath.Dir(ws), ""} {
		s := Store{Dir: dir}
		if _, _, err := s.Take(context.Background(), "sbx_1", 1, "", ws); err == nil {
			t.Fatalf("store %q was used", dir)
		}
	}
}

func TestNamesAreChecked(t *testing.T) {
	s, ws, _ := dirs(t)
	for _, n := range []string{"../x", "a b", strings.Repeat("a", 64), "x/y"} {
		if _, _, err := s.Take(context.Background(), "sbx_1", 1, n, ws); !errors.Is(err, driver.ErrInvalid) {
			t.Fatalf("name %q: %v", n, err)
		}
	}
}

// Removing a sandbox's snapshots removes the folder; one snapshot's
// removal leaves the others.
func TestRemove(t *testing.T) {
	s, ws, _ := dirs(t)
	ctx := context.Background()
	write(t, filepath.Join(ws, "a"), "x")
	r1, _, _ := s.Take(ctx, "sbx_1", 1, "", ws)
	r2, _, _ := s.Take(ctx, "sbx_1", 1, "", ws)
	if err := s.Remove("sbx_1", r1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, "sbx_1", r1, ws); !errors.Is(err, driver.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, "sbx_1", r2, ws); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAll("sbx_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "sbx_1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
