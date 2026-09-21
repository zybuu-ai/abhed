package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Reading the workspace from the console: a directory listing, one file, and
// what the agent changed.
//
// All three are read-only on purpose. A person editing a file here would change
// the workspace without an event saying so, and the record is only worth
// anything if it accounts for every change.
//
// A viewer is held to what the agent's own read is held to, and no more is
// trusted of it: the path is proven inside the workspace by tools.Session, and
// the read is put to the policy engine as the call read(path).

const (
	maxDirEntries = 2000
	maxViewBytes  = 512 << 10
)

// viewerSkip is never listed and never served. .git and node_modules are
// noise; .abhed is this server's own config and password hashes.
var viewerSkip = map[string]bool{".git": true, "node_modules": true, ".abhed": true}

var (
	errNotInView = errors.New("file not found")
	errDenied    = errors.New("reading this path is denied by policy")
)

// workspaceView is one request's scoped, policy-checked window on the workspace.
type workspaceView struct {
	sess *tools.Session
	pol  *policy.Engine
	root *os.Root
}

func (s *Server) openView() (*workspaceView, error) {
	// A fresh session rather than the live one: it carries no additional
	// roots, so the viewer is confined to the workspace itself.
	sess, err := tools.NewSession(s.opts.Workspace)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(sess.Root)
	if err != nil {
		return nil, err
	}
	// The mode decides how a mutation gets approved, and a viewer makes none.
	// What is left of the evaluation is the hooks and the deny and ask rules.
	return &workspaceView{sess: sess, pol: s.newPolicy(policy.ModeDefault), root: root}, nil
}

func (v *workspaceView) Close() { _ = v.root.Close() }

// resolve turns a client path into one relative to the workspace root, with
// symlinks followed, or refuses it.
func (v *workspaceView) resolve(p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(v.sess.Root, filepath.FromSlash(p))
	}
	clean, err := v.sess.Resolve(p)
	if err != nil {
		return "", errNotInView
	}
	real := realPath(clean)
	rel, err := filepath.Rel(v.sess.Root, real)
	if err != nil || !filepath.IsLocal(rel) {
		return "", errNotInView
	}
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if viewerSkip[part] {
				return "", errNotInView
			}
		}
	}
	info, err := os.Stat(real)
	dir := err == nil && info.IsDir()
	// Both spellings are judged: a rule written against .env must still hold
	// for a link called notes.txt that points at it.
	if !v.allowed(clean, dir) || !v.allowed(real, dir) {
		return "", errDenied
	}
	return rel, nil
}

// allowed puts the read to the policy engine as the agent's read call would be.
func (v *workspaceView) allowed(abs string, dir bool) bool {
	subjects := []string{abs}
	if dir {
		// A rule such as read(**/.ssh/**) names what is inside the directory.
		subjects = append(subjects, abs+string(filepath.Separator))
	}
	for _, subject := range subjects {
		args, _ := json.Marshal(map[string]string{"path": subject})
		if v.pol.Evaluate("read", false, args).Decision != policy.Allow {
			return false
		}
	}
	return true
}

// realPath follows symlinks in the part of p that exists, so a path to a
// deleted file still lands under the resolved root.
func realPath(p string) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func writeViewError(w http.ResponseWriter, err error) {
	if errors.Is(err, errDenied) {
		WriteError(w, http.StatusForbidden, err.Error())
		return
	}
	// One answer for "outside the workspace" and "not there": a distinct reply
	// would confirm what exists on the other side of the boundary.
	WriteError(w, http.StatusNotFound, errNotInView.Error())
}

// view runs the checks every workbench endpoint shares.
func (s *Server) view(w http.ResponseWriter, r *http.Request) (*workspaceView, bool) {
	id := r.PathValue("id")
	if !validSessionID(id) || !s.mayAccess(r, id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return nil, false
	}
	v, err := s.openView()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "workspace is not readable")
		return nil, false
	}
	return v, true
}

type treeEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

type treeResponse struct {
	Path      string      `json:"path"`
	Entries   []treeEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

// treeSession lists one directory. The console asks again as folders are
// opened, so a large repository costs what is looked at, not its whole size.
func (s *Server) treeSession(w http.ResponseWriter, r *http.Request) {
	v, ok := s.view(w, r)
	if !ok {
		return
	}
	defer v.Close()

	rel, err := v.resolve(r.URL.Query().Get("path"))
	if err != nil {
		writeViewError(w, err)
		return
	}
	dir, err := v.root.Open(rel)
	if err != nil {
		writeViewError(w, errNotInView)
		return
	}
	defer dir.Close()
	listed, err := dir.ReadDir(maxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		writeViewError(w, errNotInView)
		return
	}

	out := treeResponse{Path: filepath.ToSlash(rel), Entries: []treeEntry{}}
	if len(listed) > maxDirEntries {
		listed, out.Truncated = listed[:maxDirEntries], true
	}
	for _, e := range listed {
		if viewerSkip[e.Name()] {
			continue
		}
		child := filepath.Join(rel, e.Name())
		// Resolved as a request for it would be, so nothing is listed that
		// would then be refused: a link leading outside, a denied path.
		target, err := v.resolve(child)
		if err != nil {
			continue
		}
		info, err := v.root.Stat(target)
		if err != nil {
			continue
		}
		entry := treeEntry{Name: e.Name(), Path: filepath.ToSlash(child), Dir: info.IsDir()}
		if !entry.Dir {
			entry.Size = info.Size()
		}
		out.Entries = append(out.Entries, entry)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return a.Name < b.Name
	})
	WriteJSON(w, http.StatusOK, out)
}

type fileResponse struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Content   string `json:"content"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	// Hash names the content an editor loaded, so a save can tell it is stale.
	Hash string `json:"hash,omitempty"`
}

// fileSession returns one file as JSON. Never as a document of its own type:
// a workspace file is whatever the agent wrote, and this origin holds the
// session cookie.
func (s *Server) fileSession(w http.ResponseWriter, r *http.Request) {
	v, ok := s.view(w, r)
	if !ok {
		return
	}
	defer v.Close()

	path := r.URL.Query().Get("path")
	if strings.TrimSpace(path) == "" {
		WriteError(w, http.StatusBadRequest, "path is required")
		return
	}
	rel, err := v.resolve(path)
	if err != nil {
		writeViewError(w, err)
		return
	}
	data, size, err := v.read(rel)
	if err != nil {
		writeViewError(w, errNotInView)
		return
	}
	out := fileResponse{Path: filepath.ToSlash(rel), Size: size}
	if tools.IsBinary(data) {
		out.Binary = true
		WriteJSON(w, http.StatusOK, out)
		return
	}
	if int64(len(data)) < size {
		out.Truncated = true
		data = trimPartialRune(data)
	}
	if !out.Truncated {
		out.Hash = contentHash(data)
	}
	out.Content = string(data)
	WriteJSON(w, http.StatusOK, out)
}

// read returns at most maxViewBytes of a file, and its full size. It opens
// through os.Root, which refuses a link swapped in after resolve looked.
func (v *workspaceView) read(rel string) ([]byte, int64, error) {
	f, err := v.root.Open(rel)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errNotInView
	}
	data, err := io.ReadAll(io.LimitReader(f, maxViewBytes))
	if err != nil {
		return nil, 0, err
	}
	return data, info.Size(), nil
}

// trimPartialRune drops a multi-byte character cut in half by the size cap.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size != 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

type changedFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // added | modified | deleted
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Diff    string `json:"diff"`
	// Note says why there is no diff: binary, too large, denied, outside.
	Note string `json:"note,omitempty"`
}

type changesResponse struct {
	// Available is false when this process does not hold the session's undo
	// log: the originals live in memory, with the run.
	Available bool          `json:"available"`
	Files     []changedFile `json:"files"`
}

// changesSession reports what the agent changed, as a diff of each file's
// content before its first edit against what is on disk now.
func (s *Server) changesSession(w http.ResponseWriter, r *http.Request) {
	v, ok := s.view(w, r)
	if !ok {
		return
	}
	defer v.Close()

	out := changesResponse{Files: []changedFile{}}
	live, found := s.session(r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !found || live.undo == nil {
		WriteJSON(w, http.StatusOK, out)
		return
	}
	out.Available = true

	for _, abs := range live.undo.Changed() {
		before, existed, _ := live.undo.Original(abs)
		rel, err := v.resolve(abs)
		if err != nil {
			// Named, because the agent did change it; not shown, because the
			// viewer's boundary is the same whichever endpoint is asked.
			note := "outside the workspace"
			if errors.Is(err, errDenied) {
				note = "denied by policy"
			}
			out.Files = append(out.Files, changedFile{Path: abs, Status: "modified", Note: note})
			continue
		}
		entry := changedFile{Path: filepath.ToSlash(rel), Status: "modified"}
		after, size, err := v.read(rel)
		switch {
		case err != nil && !existed:
			continue // created and removed again: nothing changed
		case err != nil:
			entry.Status = "deleted"
		case !existed:
			entry.Status = "added"
		}
		switch {
		case size > maxViewBytes || len(before) > maxViewBytes:
			entry.Note = "too large to diff"
		case tools.IsBinary(before) || tools.IsBinary(after):
			entry.Note = "binary"
		case err == nil && existed && string(before) == string(after):
			continue
		default:
			entry.Diff, entry.Added, entry.Removed = unifiedDiff(entry.Path, string(before), string(after))
		}
		out.Files = append(out.Files, entry)
	}
	WriteJSON(w, http.StatusOK, out)
}
