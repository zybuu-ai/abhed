package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// What the person at the workbench does by hand — saving a file, running a
// command — goes through agent.Loop.Manual: the same policy, the same sandbox
// and the same record as the agent's own calls. There is no second, softer path.

// maxManualCommand bounds a command line typed into the workbench.
const maxManualCommand = 8 << 10

// manualSession finds the live session and the person's own tool session on it.
func (s *Server) manualSession(w http.ResponseWriter, r *http.Request) (*liveSession, *tools.Session, bool) {
	id := r.PathValue("id")
	if !validSessionID(id) {
		WriteError(w, http.StatusNotFound, "session not found")
		return nil, nil, false
	}
	if s.draining.Load() {
		WriteError(w, http.StatusServiceUnavailable, errDraining.Error())
		return nil, nil, false
	}
	live, found := s.session(id, TenantOf(r.Context()), UserOf(r.Context()))
	if !found {
		// A session from before a restart is continued from its record, as a
		// message would continue it, so there is a sandbox to work in.
		if !s.mayAccess(r, id) {
			WriteError(w, http.StatusNotFound, "session not found")
			return nil, nil, false
		}
		resumed, err := s.resumeSession(r.Context(), id, "", UserOf(r.Context()), TenantOf(r.Context()))
		if errors.Is(err, errBusySession) {
			// Another request may have reopened it a moment ago.
			if l, ok := s.session(id, TenantOf(r.Context()), UserOf(r.Context())); ok {
				resumed, err = l, nil
			}
		}
		switch {
		case errors.Is(err, errNoSession):
			WriteError(w, http.StatusNotFound, "session not found")
			return nil, nil, false
		case errors.Is(err, errDraining):
			w.Header().Set("Retry-After", "5")
			WriteError(w, http.StatusServiceUnavailable, errDraining.Error())
			return nil, nil, false
		case err != nil:
			s.log.Warn("could not reopen session", "session", id, "error", err)
			WriteError(w, http.StatusConflict, "this session could not be reopened on this server; start a new one")
			return nil, nil, false
		}
		live = resumed
		live.mu.Lock()
		if live.Turns == 0 && live.State == "done" {
			live.State = "idle" // a workbench session nobody has messaged yet
		}
		live.mu.Unlock()
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.manual == nil {
		live.manual = live.Loop.Session.Fork()
		if live.manual.Syntax == tools.SyntaxRefuse {
			live.manual.Syntax = tools.SyntaxReport // a person's save of work in progress is warned, never refused
		}
	}
	return live, live.manual, true
}

type saveRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// Base is the hash of the content the editor loaded, empty for a new file.
	Base string `json:"base"`
}

type saveResponse struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// saveFile writes one file for the person at the keyboard.
func (s *Server) saveFile(w http.ResponseWriter, r *http.Request) {
	live, sess, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSaveBytes)
	var req saveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "the file is too large to save from the workbench, or the request is malformed")
		return
	}
	v, err := s.openView()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "workspace is not readable")
		return
	}
	defer v.Close()
	// The view's rules come first: the harness's own state, .git and anything
	// a read rule withholds cannot be written by hand either.
	rel, err := v.resolve(req.Path)
	if err != nil {
		writeViewError(w, err)
		return
	}
	abs := filepath.Join(v.sess.Root, rel)

	// Refuse to write over a file that changed since the editor loaded it.
	current, err := os.ReadFile(abs) // #nosec G304 -- resolved inside the workspace and judged by policy above
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if req.Base != "" {
			WriteError(w, http.StatusConflict, "the file was removed since you opened it")
			return
		}
	case err != nil:
		writeViewError(w, errNotInView)
		return
	case contentHash(current) != req.Base:
		WriteError(w, http.StatusConflict, "the file changed since you opened it; reload it before saving")
		return
	default:
		sess.MarkRead(abs, string(current))
	}

	live.manualMu.Lock()
	defer live.manualMu.Unlock()
	args, _ := json.Marshal(map[string]string{"path": abs, "content": req.Content})
	res, err := live.Loop.Manual(r.Context(), sess, "write", "u"+newSessionID(), args)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "the save could not be recorded, so it was not made")
		return
	}
	if res.IsError {
		WriteError(w, http.StatusForbidden, res.Content)
		return
	}
	WriteJSON(w, http.StatusOK, saveResponse{Path: filepath.ToSlash(rel), Hash: contentHash([]byte(req.Content))})
}

type execRequest struct {
	Command string `json:"command"`
}

type execResponse struct {
	Output     string `json:"output"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	IsError    bool   `json:"is_error"`
	Truncated  bool   `json:"truncated"`
	Cwd        string `json:"cwd"`
	DurationMS int64  `json:"duration_ms"`
}

// execCommand runs one command for the person at the keyboard, in the
// session's sandbox. It is a command runner, not a terminal: no tty, no input.
func (s *Server) execCommand(w http.ResponseWriter, r *http.Request) {
	live, sess, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	capBody(w, r)
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Command) == "" {
		WriteError(w, http.StatusBadRequest, "command is required")
		return
	}
	if len(req.Command) > maxManualCommand {
		WriteError(w, http.StatusBadRequest, "command is too long")
		return
	}
	live.manualMu.Lock()
	defer live.manualMu.Unlock()
	args, _ := json.Marshal(map[string]string{"command": req.Command, "description": "typed into the workbench"})
	start := time.Now()
	res, err := live.Loop.Manual(r.Context(), sess, "bash", "u"+newSessionID(), args)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "the command could not be recorded")
		return
	}
	WriteJSON(w, http.StatusOK, execResponse{
		Output: res.Content, ExitCode: res.ExitCode, IsError: res.IsError, Truncated: res.Truncated,
		Cwd: sess.Rel(sess.Cwd), DurationMS: time.Since(start).Milliseconds(),
	})
}
