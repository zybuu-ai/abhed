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
	maxViewBytes  = 4 << 20
)

// viewerSkip is never listed and never served. .git and node_modules are
// noise; .abhed is this server's own config and password hashes. Names are
// matched without case, as a case-insensitive disk opens them.
var viewerSkip = map[string]bool{".git": true, "node_modules": true, ".abhed": true}

func skipInView(name string) bool { return viewerSkip[strings.ToLower(name)] }

var (
	errNotInView = errors.New("file not found")
	errDenied    = errors.New("reading this path is denied by policy")
)

// workspaceView is one request's scoped, policy-checked window on the workspace.
type workspaceView struct {
	sess  *tools.Session
	pol   *policy.Engine
	root  *os.Root
	state *tools.StateSet
	conf  *tools.Confined
}

func (s *Server) openView(workspace string) (*workspaceView, error) {
	// A fresh session rather than the live one: it carries no additional
	// roots, so the viewer is confined to the workspace itself.
	sess, err := tools.NewSession(workspace)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(sess.Root)
	if err != nil {
		return nil, err
	}
	// The mode decides how a mutation gets approved, and a viewer makes none.
	// What is left of the evaluation is the hooks and the deny and ask rules.
	return &workspaceView{sess: sess, pol: s.newPolicyIn(policy.ModeDefault, workspace), root: root, state: sess.FreezeState()}, nil
}

func (v *workspaceView) Close() {
	_ = v.root.Close()
	if v.conf != nil {
		v.conf.Close()
	}
}

// confined is the view's root for opens that must stay inside it and away
// from Abhed's state, opened on first use.
func (v *workspaceView) confined() *tools.Confined {
	if v.conf == nil {
		c, err := v.state.Confine(v.sess.Root)
		if err != nil {
			return nil
		}
		v.conf = c
	}
	return v.conf
}

// readAll reads a whole file of the view, as confined as read.
func (v *workspaceView) readAll(rel string) ([]byte, error) {
	c := v.confined()
	if c == nil {
		return nil, errNotInView
	}
	return c.ReadFile(filepath.Join(v.sess.Root, rel))
}

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
			if skipInView(part) {
				return "", errNotInView
			}
		}
	}
	// Abhed's state by any spelling or link, judged as the agent's read is.
	if v.state.Has(clean) || v.state.Has(real) {
		return "", errNotInView
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

// typed is p as the person named it, links not followed. A write rule written
// against a link to a folder names this spelling, which resolve follows away.
func (v *workspaceView) typed(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(v.sess.Root, filepath.FromSlash(p))
}

// writeDenial puts each spelling to the write rules, as the agent's write
// would be, and returns the first deny.
func writeDenial(pol *policy.Engine, spellings ...string) *policy.Result {
	for _, p := range spellings {
		args, _ := json.Marshal(map[string]string{"path": p})
		if d := pol.Evaluate("write", true, args); d.Decision == policy.Deny {
			return &d
		}
	}
	return nil
}

// realPath follows symlinks in the part of p that exists; see tools.RealPath.
func realPath(p string) string { return tools.RealPath(p) }

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
	ws, err := s.sessionWorkspace(r.Context(), id)
	if err != nil {
		WriteError(w, http.StatusNotFound, "the session's workspace is gone")
		return nil, false
	}
	v, err := s.openView(ws)
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
	if info, err := dir.Stat(); err != nil || v.state.HasFile(info) {
		writeViewError(w, errNotInView)
		return
	}
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
		if skipInView(e.Name()) {
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
	// The path was judged before the open, and a link can be swapped in
	// between. What was opened is judged too: it must be what the confined
	// open finds for the same path, following links only inside the root.
	c := v.confined()
	if c == nil {
		return nil, 0, errNotInView
	}
	g, err := c.OpenRead(filepath.Join(v.sess.Root, rel))
	if err != nil {
		return nil, 0, errNotInView
	}
	checked, err := g.Stat()
	_ = g.Close()
	if err != nil || v.state.HasFile(info) || !os.SameFile(info, checked) {
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
	// ByCommand marks a change the edit tools did not make: a shell command's.
	ByCommand bool `json:"by_command,omitempty"`
}

type changesResponse struct {
	// Available is false when this process does not hold the session's undo
	// log: the originals live in memory, with the run.
	Available bool          `json:"available"`
	Files     []changedFile `json:"files"`
	// Partial says why changes made by commands may be missing, when they may.
	Partial string `json:"partial,omitempty"`
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
	live, found := s.session(r.Context(), r.PathValue("id"), TenantOf(r.Context()), UserOf(r.Context()))
	if !found || live.undo == nil {
		WriteJSON(w, http.StatusOK, out)
		return
	}
	out.Available = true

	byTools := map[string]bool{}
	for _, abs := range live.undo.Changed() {
		before, existed, _ := live.undo.Original(abs)
		for _, p := range []string{abs, realPath(abs)} {
			if r, err := filepath.Rel(v.sess.Root, p); err == nil && filepath.IsLocal(r) {
				byTools[relSlash(r)] = true
			}
		}
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
	shell, partial := shellChanges(v, live.snap, byTools)
	out.Files, out.Partial = append(out.Files, shell...), partial
	WriteJSON(w, http.StatusOK, out)
}

// shellChanges are the files changed since the session opened that the edit
// tools' undo log does not hold: by a command, the person's or the agent's.
func shellChanges(v *workspaceView, snap *wsSnapshot, byTools map[string]bool) ([]changedFile, string) {
	if snap == nil {
		return nil, ""
	}
	changes, partial := snap.changes(byTools)
	out := make([]changedFile, 0, len(changes))
	for _, c := range changes {
		rel, err := v.resolve(c.rel)
		if err != nil {
			if errors.Is(err, errDenied) {
				out = append(out, changedFile{Path: c.rel, Status: c.status, Note: "denied by policy", ByCommand: true})
			}
			continue
		}
		entry := changedFile{Path: filepath.ToSlash(rel), Status: c.status, ByCommand: true}
		if c.diffable {
			entry.Diff, entry.Added, entry.Removed = unifiedDiff(entry.Path, string(c.before), string(c.after))
		} else {
			entry.Note = "binary, too large, or no copy kept from when the session opened"
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, partial
}
