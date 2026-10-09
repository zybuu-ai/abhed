package sandboxcli

import (
	"errors"
	"fmt"
	"io"
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
		"skipped": st.Skipped}, func(tw *tabwriter.Writer) {
		fmt.Fprintf(tw, "copied %d entries (%d bytes) into %s:%s", st.Entries, st.Bytes, id, dst)
		if st.Skipped > 0 {
			fmt.Fprintf(tw, "; %d links, sockets or devices left out", st.Skipped)
		}
		fmt.Fprintln(tw)
	})
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
	if err != nil {
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
	st, err := archive.Unpack(r.Ctx, resp.Body, root, archive.Limits{}, nil)
	if err != nil {
		return fmt.Errorf("copying %s:%s out stopped after %d entries: %w", id, src, st.Entries, err)
	}
	// The trailer arrives once the body has been read to its end.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if e := resp.Trailer.Get("X-Abhed-Error"); e != "" {
		return fmt.Errorf("copying %s:%s out failed after %d entries: %s", id, src, st.Entries, e)
	}
	return r.emit(map[string]any{"direction": "out", "sandbox_id": id, "path": src, "entries": st.Entries, "bytes": st.Bytes},
		func(tw *tabwriter.Writer) {
			fmt.Fprintf(tw, "copied %d entries (%d bytes) from %s:%s into %s\n", st.Entries, st.Bytes, id, src, dst)
		})
}
