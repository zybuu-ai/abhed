package bgcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/bgjob"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/regularfile"
	"github.com/zybuu-ai/abhed/internal/secretfiles"
)

// packed is a snapshot of a repository's tracked files as they are in the
// working tree, and what was left out of it and why.
type packed struct {
	tgz   []byte
	files int
	bytes int64
	// withheld are files the person's read rules keep from the agent;
	// secret are files named like a key or holding one; skipped are links,
	// submodules and files over the per-file limit.
	withheld, secret, skipped []string
}

// errNotARepository is a workspace that is not inside a git repository.
var errNotARepository = errors.New("not in a git repository")

// repoRoot is the top of the repository dir is in.
func repoRoot(ctx context.Context, dir string) (string, error) {
	out, err := hostgit.Command(ctx, dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", errNotARepository
	}
	return filepath.Clean(strings.TrimSpace(string(out))), nil
}

// snapshotEntry is one tracked path and its index mode.
type snapshotEntry struct {
	path string
	mode string
}

// trackedFiles lists the repository's tracked paths with their modes, each
// once, from the index: what HEAD has plus what is staged.
func trackedFiles(ctx context.Context, root string) ([]snapshotEntry, error) {
	out, err := hostgit.Command(ctx, root, "ls-files", "-z", "--cached", "--stage").Output()
	if err != nil {
		return nil, fmt.Errorf("list the repository's files: %w", err)
	}
	seen := map[string]bool{}
	var list []snapshotEntry
	for _, rec := range bytes.Split(out, []byte{0}) {
		meta, name, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			continue
		}
		f := strings.Fields(string(meta)) // mode, object, stage
		if len(f) != 3 || seen[string(name)] {
			continue
		}
		seen[string(name)] = true
		list = append(list, snapshotEntry{path: string(name), mode: f[0]})
	}
	sort.Slice(list, func(a, b int) bool { return list[a].path < list[b].path })
	return list, nil
}

// fit reports whether a tracked path may go into a snapshot by its name:
// relative, slash-separated, valid text, no "." or ".." part, and nothing
// under .git or .abhed.
func fit(p string) bool {
	if p == "" || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		switch strings.ToLower(part) {
		case "", ".", "..", ".git", ".abhed":
			return false
		}
	}
	return path.Clean(p) == p
}

// pack builds a snapshot of root's tracked files as they are in the working
// tree, so staged and unstaged edits go with them and untracked files do
// not. A file the read rules withhold, named like a key or holding one, or
// holding a value from the person's secrets store, is left out and named.
func pack(ctx context.Context, root string, withheld func(abs string) bool, holdsStored func([]byte) bool) (packed, error) {
	var p packed
	list, err := trackedFiles(ctx, root)
	if err != nil {
		return p, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return p, err
	}
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	for _, e := range list {
		if err := ctx.Err(); err != nil {
			return p, err
		}
		if !fit(e.path) {
			p.skipped = append(p.skipped, e.path+" (its name)")
			continue
		}
		switch e.mode {
		case "100644", "100755":
		case "120000":
			p.skipped = append(p.skipped, e.path+" (a link)")
			continue
		case "160000":
			p.skipped = append(p.skipped, e.path+" (a submodule)")
			continue
		default:
			p.skipped = append(p.skipped, e.path+" (mode "+e.mode+")")
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(e.path))
		if withheld != nil && withheld(abs) {
			p.withheld = append(p.withheld, e.path)
			continue
		}
		if secretfiles.Match(e.path) != "" {
			p.secret = append(p.secret, e.path)
			continue
		}
		fi, err := os.Lstat(abs)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // deleted in the working tree
		case err != nil:
			return p, err
		case !fi.Mode().IsRegular():
			p.skipped = append(p.skipped, e.path+" (not a regular file now)")
			continue
		case fi.Size() > bgjob.MaxSnapshotFile:
			p.skipped = append(p.skipped, fmt.Sprintf("%s (over %d MiB)", e.path, bgjob.MaxSnapshotFile>>20))
			continue
		}
		// A folder in the path swapped for a link would read outside the repository.
		if real, err := filepath.EvalSymlinks(abs); err != nil || !inside(realRoot, real) {
			p.skipped = append(p.skipped, e.path+" (reached through a link)")
			continue
		}
		data, err := regularfile.Read(abs, bgjob.MaxSnapshotFile)
		if err != nil {
			return p, err
		}
		if secretfiles.HasKeyMaterial(string(data)) || (holdsStored != nil && holdsStored(data)) {
			p.secret = append(p.secret, e.path)
			continue
		}
		if p.files+1 > bgjob.MaxSnapshotFiles || p.bytes+int64(len(data)) > bgjob.MaxSnapshotTotal {
			return p, fmt.Errorf("the repository's tracked files are over a snapshot's limit (%d files, %d MiB)",
				bgjob.MaxSnapshotFiles, bgjob.MaxSnapshotTotal>>20)
		}
		mode := int64(0o644)
		if fi.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: e.path, Mode: mode, Size: int64(len(data)),
			ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return p, err
		}
		if _, err := tw.Write(data); err != nil {
			return p, err
		}
		p.files++
		p.bytes += int64(len(data))
		if raw.Len() > bgjob.MaxSnapshotUpload {
			return p, fmt.Errorf("the snapshot is over the %d MiB a server takes compressed", bgjob.MaxSnapshotUpload>>20)
		}
	}
	if err := tw.Close(); err != nil {
		return p, err
	}
	if err := zw.Close(); err != nil {
		return p, err
	}
	if raw.Len() > bgjob.MaxSnapshotUpload {
		return p, fmt.Errorf("the snapshot is over the %d MiB a server takes compressed", bgjob.MaxSnapshotUpload>>20)
	}
	p.tgz = raw.Bytes()
	return p, nil
}

// inside reports whether p is root or under it.
func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && filepath.IsLocal(rel)
}
