package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A subagent's worktree is taken back from the workbench by hunk: the list
// shows each worktree's changes against the commit it shares with the
// session's workspace, and a take puts the accepted hunks in as the person's
// own writes, through Loop.Manual, with the view's rules first.

type worktreesResponse struct {
	Worktrees []agent.WorktreeChanges `json:"worktrees"`
}

// listWorktrees answers GET /v1/sessions/{id}/worktrees.
func (s *Server) listWorktrees(w http.ResponseWriter, r *http.Request) {
	v, ok := s.view(w, r)
	if !ok {
		return
	}
	defer v.Close()
	out := worktreesResponse{Worktrees: []agent.WorktreeChanges{}}
	list, err := agent.ListWorktrees(r.Context(), v.sess.Root)
	if err != nil {
		// Not a repository, or no git: there is nothing to take.
		WriteJSON(w, http.StatusOK, out)
		return
	}
	for _, wt := range list {
		ch, err := wt.Changes(r.Context(), v.sess.Root)
		if err != nil {
			continue
		}
		for i, f := range ch.Files {
			// What a read rule withholds is named, not shown, in the worktree as in the workspace.
			_, rerr := v.resolve(f.Path)
			if errors.Is(rerr, errDenied) || !v.allowed(filepath.Join(wt.Dir, filepath.FromSlash(f.Path)), false) {
				ch.Files[i].Hunks, ch.Files[i].Unsupported = nil, "denied by policy"
			}
		}
		out.Worktrees = append(out.Worktrees, ch)
	}
	WriteJSON(w, http.StatusOK, out)
}

type takeRequest struct {
	// Accept names, by the workspace's slash path, the hunks to take, by the
	// index the list gave them.
	Accept map[string][]int `json:"accept"`
	// Remove removes the worktree and its branch afterwards.
	Remove bool `json:"remove"`
}

// takeWorktree answers POST /v1/sessions/{id}/worktrees/{wt}/take.
func (s *Server) takeWorktree(w http.ResponseWriter, r *http.Request) {
	live, sess, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	capBody(w, r)
	var req takeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, "the request is malformed")
		return
	}
	v, err := s.openView(s.workspaceOf(live))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "workspace is not readable")
		return
	}
	defer v.Close()
	wt, err := agent.FindWorktree(r.Context(), v.sess.Root, r.PathValue("wt"))
	if err != nil {
		WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	live.manualMu.Lock()
	defer live.manualMu.Unlock()
	res, err := live.Loop.Take(r.Context(), v.sess.Root, wt, req.Accept, req.Remove,
		viewTaker{v: v, inner: live.Loop.ManualTaker(sess), pol: live.Loop})
	if err != nil {
		writeUnrecorded(w, err, "the take could not be made")
		return
	}
	WriteJSON(w, http.StatusOK, res)
}

// viewTaker is the person's TakeWriter held to the view: a path the view
// hides or a read rule withholds is refused, and a write rule on the path as
// named holds as for a save.
type viewTaker struct {
	v     *workspaceView
	inner agent.TakeWriter
	pol   *agent.Loop
}

func (t viewTaker) check(rel string) error {
	if _, err := t.v.resolve(rel); err != nil {
		return err
	}
	if d := writeDenial(t.pol.Policy, t.v.typed(rel)); d != nil {
		return errors.New("Denied: " + d.Reason)
	}
	return nil
}

func (t viewTaker) ReadFile(rel string) ([]byte, bool, error) {
	if _, err := t.v.resolve(rel); err != nil {
		return nil, false, err
	}
	data, err := t.v.readAll(filepath.FromSlash(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (t viewTaker) WriteFile(ctx context.Context, rel string, content []byte) error {
	if err := t.check(rel); err != nil {
		return err
	}
	return t.inner.WriteFile(ctx, rel, content)
}

func (t viewTaker) RemoveFile(ctx context.Context, rel string) error {
	if err := t.check(rel); err != nil {
		return err
	}
	return t.inner.RemoveFile(ctx, rel)
}
