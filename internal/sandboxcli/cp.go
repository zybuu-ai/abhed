package sandboxcli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/driver/archive"
)

// remoteSpec splits "sbx_…:PATH"; ok is false for a local path.
func remoteSpec(s string) (id, p string, ok bool) {
	id, p, found := strings.Cut(s, ":")
	if !found || !controlplane.ValidID(id) {
		return "", "", false
	}
	return id, p, true
}

// inState reports whether a path names or lies in a .abhed folder, in any
// case: Abhed's state is never copied in or out.
func inState(p string) bool {
	return archive.InState(filepath.ToSlash(filepath.Clean(p)))
}

func (r *runner) cp(args []string) error {
	fs := r.flags("cp", "SRC DST")
	pos, err := r.parse(fs, args, 0)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError{errors.New("cp takes a source and a destination, one of them ID:PATH")}
	}
	sid, spath, sRemote := remoteSpec(pos[0])
	did, dpath, dRemote := remoteSpec(pos[1])
	switch {
	case sRemote == dRemote:
		return usageError{errors.New("cp copies between this machine and a sandbox: exactly one side is ID:PATH")}
	case dRemote:
		return r.copyIn(pos[0], did, dpath)
	default:
		return r.copyOut(sid, spath, pos[1])
	}
}

func checkRemotePath(p string) error {
	if p == "" {
		return usageError{errors.New("name a path in the sandbox's workspace after the id, such as ID:src")}
	}
	if archive.InState(path.Clean(p)) {
		return usageError{fmt.Errorf("%s is Abhed's state, which is never copied", p)}
	}
	return nil
}

// copyIn sends a local file or folder, as a tar with its own name at the
// top, into a folder in the sandbox.
func (r *runner) copyIn(src, id, dst string) error {
	if err := checkRemotePath(dst); err != nil {
		return err
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	if inState(abs) {
		return usageError{fmt.Errorf("%s is Abhed's state, which is never copied", src)}
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return usageError{fmt.Errorf("%s is a link; name what it points to", src)}
	}
	if !fi.IsDir() && !fi.Mode().IsRegular() {
		return usageError{fmt.Errorf("%s is neither a file nor a folder", src)}
	}
	parent, base := filepath.Dir(abs), filepath.Base(abs)
	if base == string(filepath.Separator) || base == "." {
		return usageError{fmt.Errorf("%s: name a file or folder, not the root", src)}
	}
	if err := r.connectClient(); err != nil {
		return err
	}
	// Signed in before the upload begins, so it is not sent twice.
	if _, _, err := r.getSandbox(id); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	var st archive.Stats
	go func() {
		var err error
		st, err = archive.Pack(r.Ctx, parent, pw, archive.Limits{}, func(name string) bool {
			return name != base && !strings.HasPrefix(name, base+"/")
		})
		pw.CloseWithError(err)
	}()
	q := url.Values{"direction": {"in"}, "path": {dst}}
	resp, err := r.cl.do(r.Ctx, call{method: http.MethodPost, path: sandboxPath(id, "cp") + "?" + q.Encode(),
		stream: pr, contentType: "application/x-tar", mutating: true})
	_ = pr.CloseWithError(errors.New("the upload ended"))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return r.emit(map[string]any{"direction": "in", "sandbox_id": id, "path": dst, "entries": st.Entries, "bytes": st.Bytes,
		"skipped": st.Skipped, "state_skipped": st.State}, func(tw *tabwriter.Writer) {
		fmt.Fprintf(tw, "copied %d entries (%d bytes) into %s:%s%s\n", st.Entries, st.Bytes, id, dst, leftOut(st))
	})
}

// leftOut says what a pack left out, "" when nothing.
func leftOut(st archive.Stats) string {
	var parts []string
	if st.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d links, sockets or devices", st.Skipped))
	}
	if st.State > 0 {
		parts = append(parts, fmt.Sprintf("%d .abhed (Abhed's state, never copied)", st.State))
	}
	if len(parts) == 0 {
		return ""
	}
	return "; " + strings.Join(parts, " and ") + " left out"
}

// copyOut unpacks what the sandbox sends into a local folder. The archive
// is untrusted: every entry is checked as the server checks a copy in, and
// it is written through an os.Root, which no link or ".." can leave; no
// existing file is overwritten and nothing is written into .abhed.
func (r *runner) copyOut(id, src, dst string) error {
	if err := checkRemotePath(src); err != nil {
		return err
	}
	abs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if inState(abs) {
		return usageError{fmt.Errorf("%s is Abhed's state, which is never written", dst)}
	}
	fi, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return usageError{fmt.Errorf("%s does not exist; cp out copies into a folder that does", dst)}
	} else if err != nil {
		return err
	}
	if !fi.IsDir() {
		return usageError{fmt.Errorf("%s is not a folder; cp out copies into a folder", dst)}
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := r.connectClient(); err != nil {
		return err
	}
	body := []byte(fmt.Sprintf(`{"direction":"out","path":%q}`, src))
	resp, err := r.cl.do(r.Ctx, call{method: http.MethodPost, path: sandboxPath(id, "cp"), body: body, mutating: true})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	before := topNames(root)
	st, err := archive.Unpack(r.Ctx, resp.Body, root, archive.Limits{}, nil)
	if err != nil {
		removeEmptyNew(root, before)
		return fmt.Errorf("copying %s:%s out stopped after %d entries: %w", id, src, st.Entries, err)
	}
	// The trailer arrives once the body has been read to its end.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if e := resp.Trailer.Get("X-Abhed-Error"); e != "" {
		removeEmptyNew(root, before)
		return fmt.Errorf("copying %s:%s out failed after %d entries: %s", id, src, st.Entries, e)
	}
	return r.emit(map[string]any{"direction": "out", "sandbox_id": id, "path": src, "entries": st.Entries, "bytes": st.Bytes},
		func(tw *tabwriter.Writer) {
			fmt.Fprintf(tw, "copied %d entries (%d bytes) from %s:%s into %s\n", st.Entries, st.Bytes, id, src, dst)
		})
}

// topNames are the names in root's folder, nil when it cannot be read.
func topNames(root *os.Root) map[string]bool {
	es, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(es))
	for _, e := range es {
		out[e.Name()] = true
	}
	return out
}

// removeEmptyNew removes the folders a failed copy out made at the top of
// root that hold nothing but folders, so it leaves no empty shell behind.
// A file it wrote is kept, as is everything that was there before.
func removeEmptyNew(root *os.Root, before map[string]bool) {
	if before == nil {
		return
	}
	for name := range topNames(root) {
		if before[name] || !onlyFolders(root, name) {
			continue
		}
		_ = root.RemoveAll(name)
	}
}

// onlyFolders reports whether name is a folder whose tree holds only folders.
func onlyFolders(root *os.Root, name string) bool {
	ok := true
	err := fs.WalkDir(root.FS(), name, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || !e.IsDir() {
			ok = false
			return fs.SkipAll
		}
		return nil
	})
	return ok && err == nil
}
