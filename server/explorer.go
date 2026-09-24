package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Creating a folder, renaming and deleting from the Explorer have no tool of
// their own. Each is one shell command run through agent.Loop.Manual, so it has
// the policy, the sandbox and the record of a command typed into the terminal.
// Before that, every path is held to the view's rules and to the write rules a
// save would meet, which a command line alone would not carry.

// maxExplorerEntries bounds how much of a folder is checked before it is moved or deleted.
const maxExplorerEntries = 5000

type folderRequest struct {
	Path string `json:"path"`
}

type renameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type explorerResponse struct {
	Path string `json:"path"`
}

// createFolder makes one directory, and its parents, for the person at the keyboard.
func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	s.explorerOp(w, r, &req, func(x *explorerCall) {
		abs, ok := x.target(req.Path, true)
		if !ok {
			return
		}
		x.run(abs, "mkdir -p -- "+shellQuote(abs), "created a folder in the explorer")
	})
}

// renamePath moves a file or folder within the workspace. It never replaces
// what is already at the destination.
func (s *Server) renamePath(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	s.explorerOp(w, r, &req, func(x *explorerCall) {
		from, ok := x.source(req.From)
		if !ok {
			return
		}
		info, err := os.Lstat(from)
		to, ok := x.target(req.To, err == nil && info.IsDir())
		if !ok {
			return
		}
		x.run(to, "mv -n -- "+shellQuote(from)+" "+shellQuote(to), "renamed in the explorer")
	})
}

// deletePath removes a file, or a folder and everything in it.
func (s *Server) deletePath(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	s.explorerOp(w, r, &req, func(x *explorerCall) {
		abs, ok := x.source(req.Path)
		if !ok {
			return
		}
		cmd := "rm -- "
		if info, err := os.Lstat(abs); err == nil && info.IsDir() {
			cmd = "rm -r -- "
		}
		x.run(abs, cmd+shellQuote(abs), "deleted in the explorer")
	})
}

// explorerCall carries one request through its checks; the first failure
// writes the reply and ends it.
type explorerCall struct {
	s    *Server
	w    http.ResponseWriter
	r    *http.Request
	live *liveSession
	sess *tools.Session
	v    *workspaceView
	pol  *policy.Engine
	// refused is set once a reply has been written for a denied path.
	refused bool
}

func (s *Server) explorerOp(w http.ResponseWriter, r *http.Request, req any, do func(*explorerCall)) {
	live, sess, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	capBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		WriteError(w, http.StatusBadRequest, "the request is malformed")
		return
	}
	if _, found := live.Loop.Tools.Get("bash"); !found {
		WriteError(w, http.StatusForbidden, "commands are not enabled on this server, so the explorer cannot change files")
		return
	}
	v, err := s.openView()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "workspace is not readable")
		return
	}
	defer v.Close()
	do(&explorerCall{s: s, w: w, r: r, live: live, sess: sess, v: v, pol: live.Loop.Policy})
}

// lexical checks a client path against the view and returns it joined to the
// root without following a final link, so a link is renamed or removed, never its target.
func (x *explorerCall) lexical(p string) (string, bool) {
	local := filepath.FromSlash(p)
	if strings.TrimSpace(p) == "" || filepath.IsAbs(local) || !filepath.IsLocal(local) || filepath.Clean(local) == "." {
		writeViewError(x.w, errNotInView)
		return "", false
	}
	if _, err := x.v.resolve(p); err != nil {
		writeViewError(x.w, err)
		return "", false
	}
	return filepath.Join(x.v.sess.Root, local), true
}

// source is a path that must exist, and whose contents may all be written.
func (x *explorerCall) source(p string) (string, bool) {
	abs, ok := x.lexical(p)
	if !ok {
		return "", false
	}
	info, err := os.Lstat(abs)
	if err != nil {
		writeViewError(x.w, errNotInView)
		return "", false
	}
	if !x.writable(abs, info.IsDir()) {
		return "", false
	}
	return abs, true
}

// target is a path that must not exist yet; dir says a folder will be there.
func (x *explorerCall) target(p string, dir bool) (string, bool) {
	abs, ok := x.lexical(p)
	if !ok {
		return "", false
	}
	if _, err := os.Lstat(abs); err == nil {
		WriteError(x.w, http.StatusConflict, "something with that name is already there")
		return "", false
	}
	if !x.judge(abs) || (dir && !x.judge(abs+string(filepath.Separator))) {
		return "", false
	}
	return abs, true
}

var errTooMany = errors.New("too many entries")

// writable puts a write of the path, and of everything under a folder, to the policy.
func (x *explorerCall) writable(abs string, dir bool) bool {
	if !dir {
		return x.judge(abs)
	}
	seen := 0
	err := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if seen++; seen > maxExplorerEntries {
			return errTooMany
		}
		subject := p
		if d.IsDir() {
			// A rule such as write(**/locked/**) names what is inside the folder.
			subject += string(filepath.Separator)
		}
		if !x.judge(subject) {
			return fs.SkipAll
		}
		return nil
	})
	switch {
	case errors.Is(err, errTooMany):
		WriteError(x.w, http.StatusRequestEntityTooLarge, fmt.Sprintf("this folder holds more than %d entries; change it from the terminal", maxExplorerEntries))
		return false
	case err != nil:
		writeViewError(x.w, errNotInView)
		return false
	}
	return !x.refused
}

// judge refuses a path a save to it would be refused.
func (x *explorerCall) judge(abs string) bool {
	if x.refused {
		return false
	}
	args, _ := json.Marshal(map[string]string{"path": abs})
	if d := x.pol.Evaluate("write", true, args); d.Decision == policy.Deny {
		x.refused = true
		WriteError(x.w, http.StatusForbidden, "Denied: "+d.Reason)
		return false
	}
	return true
}

// run records and runs the command as the person's own action.
func (x *explorerCall) run(abs, command, description string) {
	x.live.manualMu.Lock()
	defer x.live.manualMu.Unlock()
	args, _ := json.Marshal(map[string]string{"command": command, "description": description})
	res, err := x.live.Loop.Manual(x.r.Context(), x.sess, "bash", "u"+newSessionID(), args)
	switch {
	case err != nil:
		WriteError(x.w, http.StatusInternalServerError, "the change could not be recorded, so it was not made")
	case res.IsError:
		WriteError(x.w, http.StatusForbidden, res.Content)
	case res.ExitCode != nil && *res.ExitCode != 0:
		WriteError(x.w, http.StatusConflict, strings.TrimSpace(res.Content))
	default:
		rel, _ := filepath.Rel(x.v.sess.Root, abs)
		WriteJSON(x.w, http.StatusOK, explorerResponse{Path: filepath.ToSlash(rel)})
	}
}

// shellQuote makes one word of s for /bin/sh, whatever it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
