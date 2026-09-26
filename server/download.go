package server

import (
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// downloadEntry is one file a session produced.
type downloadEntry struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// Getting a file the agent produced back out of the workspace.
//
// Abhed can write a .docx, an .xlsx, a PDF or a diagram, and until now it
// answered with a path like /workspace/report.docx — a location inside a
// container that the person reading the message has no way to open. The work
// was done and then stranded, which is worse than not doing it.
//
// The endpoint is deliberately narrow. It serves a file only if the caller owns
// the session, only from inside the workspace, and only as an attachment.

// serveDownload streams one file from the workspace to its owner.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSessionID(id) {
		WriteError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	// Ownership is checked against the session, not the file: the workspace is
	// shared, so "this file exists" must never be enough to read it.
	if !s.mayAccess(r, id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}

	rel := r.URL.Query().Get("path")
	if strings.TrimSpace(rel) == "" {
		WriteError(w, http.StatusBadRequest, "path is required")
		return
	}

	abs, err := s.resolveInWorkspace(rel)
	// The same message whether the path escaped, is withheld or simply is not
	// there: a distinct reply would confirm what exists behind the boundary.
	if err != nil || !s.mayServe(abs) || tools.IsState(s.requested(rel), s.opts.Workspace) {
		WriteError(w, http.StatusNotFound, "file not found")
		return
	}

	// The path was judged before this open, and a link can be swapped in
	// between; so the file is opened under the workspace held open as a root,
	// and what was opened is judged again.
	root, err := filepath.EvalSymlinks(s.opts.Workspace)
	if err != nil {
		WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	c, err := tools.NewStateSet(s.opts.Workspace).Confine(root, s.opts.Workspace)
	if err != nil {
		WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	defer c.Close()
	f, err := c.OpenRead(abs)
	if err != nil {
		WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		WriteError(w, http.StatusNotFound, "file not found")
		return
	}

	name := filepath.Base(abs)
	ctype := mime.TypeByExtension(filepath.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}

	// Always an attachment, never inline. A workspace file is agent-generated
	// content, and rendering, say, an .html or .svg in the browser would run
	// whatever the model wrote on this origin — the one origin that holds the
	// session cookie.
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+sanitizeFilename(name)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// mayServe decides whether a file inside the workspace may be handed to a
// person. Being inside the workspace was the only test, which let any signed-in
// user fetch .abhed/users.json — the password hashes — and any file a deny
// rule kept from the agent.
//
// abs is already symlink-resolved, so a link is judged by what it points at.
func (s *Server) mayServe(abs string) bool {
	root, err := filepath.EvalSymlinks(s.opts.Workspace)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	// The server's own state is never a deliverable, whatever the rules say,
	// under any spelling or link that reaches it.
	if !filepath.IsLocal(rel) || tools.IsState(abs, root, s.opts.Workspace) {
		return false
	}
	// Then the operator's rules, asked the way the agent's read is asked.
	// Anything short of allow is a refusal: nobody can answer a prompt here.
	args, _ := json.Marshal(map[string]string{"path": abs})
	return s.newPolicy(policy.ModeDefault).Evaluate("read", false, args).Decision == policy.Allow
}

// requested is the path a client named, joined to the workspace but with no
// link followed, so the state check sees the spelling as well as the target.
func (s *Server) requested(rel string) string {
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}
	return filepath.Join(s.opts.Workspace, rel)
}

// resolveInWorkspace turns a client-supplied path into an absolute one that is
// provably inside the workspace, or fails.
//
// The check mirrors tools.Session.Resolve rather than reimplementing it: the
// deepest existing ancestor is symlink-resolved and compared against the
// resolved root, so a symlink planted inside the workspace cannot be used to
// read something outside it. filepath.Join alone would CLEAN a "..", which
// resolves the traversal instead of refusing it.
func (s *Server) resolveInWorkspace(rel string) (string, error) {
	root, err := filepath.EvalSymlinks(s.opts.Workspace)
	if err != nil {
		return "", err
	}

	// Accept both "report.docx" and the "/workspace/report.docx" the agent
	// prints, since the second is what a user will copy out of the transcript.
	clean := rel
	if filepath.IsAbs(clean) {
		if r, err := filepath.Rel(s.opts.Workspace, clean); err == nil {
			clean = r
		}
	}

	abs := filepath.Join(root, clean)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	within, err := filepath.Rel(root, real)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", os.ErrNotExist
	}
	return real, nil
}

// sanitizeFilename keeps a Content-Disposition header from being split by a
// filename the model chose. Reuses the upload sanitiser's rules.
func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "download"
	}
	return name
}

// listDownloads reports the files a session produced, so the console can offer
// them without the user having to notice a path in the transcript.
func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSessionID(id) || !s.mayAccess(r, id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}

	root, err := filepath.EvalSymlinks(s.opts.Workspace)
	if err != nil {
		WriteJSON(w, http.StatusOK, []downloadEntry{})
		return
	}

	type entry = downloadEntry
	out := []entry{}
	// One level deep, and only the formats a person would want back. A full
	// walk of a repository would list every source file the agent read, which
	// is noise rather than output.
	ents, err := os.ReadDir(root)
	if err != nil {
		WriteJSON(w, http.StatusOK, out)
		return
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !downloadableExt[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, entry{
			Name:     e.Name(),
			Bytes:    info.Size(),
			Modified: info.ModTime().UTC(),
		})
	}
	WriteJSON(w, http.StatusOK, out)
}

// downloadableExt is what the agent produces on purpose, as opposed to what it
// happens to read. Source files are excluded deliberately: a list of every .go
// file in the repo is not "your downloads".
var downloadableExt = map[string]bool{
	".docx": true, ".xlsx": true, ".pptx": true, ".pdf": true,
	".png": true, ".jpg": true, ".jpeg": true, ".svg": true, ".gif": true,
	".csv": true, ".zip": true,
}

// deleteSession removes a session and its transcript.
//
// Users asked for this, and the reason is not tidiness: a transcript can hold a
// pasted credential, a customer name, or a document someone uploaded by
// mistake, and "you cannot remove that" is the wrong answer to give them.
//
// The store interface does not require deletion, because an append-only audit
// log in a regulated deployment must not offer it. When the backend cannot
// delete, this says so rather than returning 204 and leaving the data in place
// — a delete button that silently does nothing is a privacy bug wearing a
// feature's clothes.
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSessionID(id) {
		WriteError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if !s.mayAccess(r, id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}

	// A running session is stopped first. Deleting the transcript of a run that
	// is still appending to it would leave rows behind after the delete.
	if live, ok := s.session(id, TenantOf(r.Context()), UserOf(r.Context())); ok {
		live.mu.Lock()
		c := live.cancel
		live.mu.Unlock()
		if c != nil {
			c()
		}
		live.Cancel()
		live.closeTerminals()
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
	}

	del, ok := s.store.(agent.SessionDeleter)
	if !ok {
		WriteError(w, http.StatusNotImplemented,
			"this deployment's store is append-only; sessions cannot be deleted")
		return
	}
	if err := del.DeleteSession(id); err != nil {
		WriteError(w, http.StatusInternalServerError, "delete failed")
		return
	}

	s.log.Info("session deleted", "session", id, "user", UserOf(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}
