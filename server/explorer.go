package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// Creating a folder, renaming and deleting from the Explorer have no tool of
// their own. Each is judged and recorded as its own action (mkdir, rename,
// delete) through agent.Loop.ManualAs, not as command text, and carried out as
// one shell command in the sandbox. Before that, every path it touches, and for
// a folder everything inside it at the old name and the new, is held to the
// view's rules and to the write rules a save would meet.
//
// Checks and command run under the person's manual lock, so no other workbench
// action lands between them. The agent's own writes can still: that race is the
// one a person's rm -r in the terminal already has.

const (
	// maxExplorerEntries bounds how much of a folder is checked before it is moved or deleted.
	maxExplorerEntries = 5000
	explorerTimeout    = 2 * time.Minute
)

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

// explorerPlan is the action a request comes to, once every check has passed,
// and the command that carries it out.
type explorerPlan struct {
	action               string            // what the policy judges and the record names
	args                 map[string]string // the action's paths
	also                 []string          // further paths the action is judged by, such as a folder's contents
	removes              []string          // paths the action takes away, put to delete rules too
	command, description string
	result               string      // the workspace path reported back
	done                 func() bool // whether the change is on disk afterwards
}

// createFolder makes one directory, and its parents, for the person at the keyboard.
func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	s.explorerOp(w, r, &req, func(x *explorerCall) (explorerPlan, bool) {
		rel, abs, ok := x.named(req.Path)
		if !ok || !x.absent(abs) || !x.check(rel, true, "") {
			return explorerPlan{}, false
		}
		return explorerPlan{
			action: "mkdir", args: map[string]string{"path": abs},
			command: "mkdir -p -- " + shellQuote(abs), description: "created a folder in the explorer", result: rel,
			done: func() bool { info, err := os.Stat(abs); return err == nil && info.IsDir() },
		}, true
	})
}

// renamePath moves a file or folder within the workspace. It never replaces
// what is already at the destination.
func (s *Server) renamePath(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	s.explorerOp(w, r, &req, func(x *explorerCall) (explorerPlan, bool) {
		from, fromAbs, ok := x.named(req.From)
		if !ok {
			return explorerPlan{}, false
		}
		info, ok := x.present(fromAbs)
		if !ok {
			return explorerPlan{}, false
		}
		to, toAbs, ok := x.named(req.To)
		if !ok || !x.absent(toAbs) || !x.check(from, info.IsDir(), "") || !x.check(to, info.IsDir(), "") {
			return explorerPlan{}, false
		}
		// A rule on the action is put to the new name as well as the old, and to each entry inside.
		also := append(x.subjects(from, info.IsDir()), x.subjects(to, info.IsDir())...)
		// The old path is removed, so it and everything inside it also meet delete rules.
		removes := x.subjects(from, info.IsDir())
		// Everything inside moves too: each entry is judged where it is and where it lands.
		if info.IsDir() && !x.contents(from, func(sub string, dir bool) bool {
			removes = append(removes, x.subjects(filepath.Join(from, sub), dir)...)
			also = append(also, x.subjects(filepath.Join(from, sub), dir)...)
			also = append(also, x.subjects(filepath.Join(to, sub), dir)...)
			return x.check(filepath.Join(from, sub), dir, sub) && x.check(filepath.Join(to, sub), dir, sub)
		}) {
			return explorerPlan{}, false
		}
		return explorerPlan{
			action: "rename", args: map[string]string{"path": fromAbs, "to": toAbs}, also: also, removes: removes,
			command: "mv -n -- " + shellQuote(fromAbs) + " " + shellQuote(toAbs), description: "renamed in the explorer", result: to,
			done: func() bool { return moved(info, fromAbs, toAbs) },
		}, true
	})
}

// deletePath removes a file, or a folder and everything in it.
func (s *Server) deletePath(w http.ResponseWriter, r *http.Request) {
	var req folderRequest
	s.explorerOp(w, r, &req, func(x *explorerCall) (explorerPlan, bool) {
		rel, abs, ok := x.named(req.Path)
		if !ok {
			return explorerPlan{}, false
		}
		info, ok := x.present(abs)
		if !ok || !x.check(rel, info.IsDir(), "") {
			return explorerPlan{}, false
		}
		cmd := "rm -- "
		// A rule on the action is put to everything the delete removes, not only its top.
		also := x.subjects(rel, info.IsDir())
		if info.IsDir() {
			cmd = "rm -r -- "
			if !x.contents(rel, func(sub string, dir bool) bool {
				also = append(also, x.subjects(filepath.Join(rel, sub), dir)...)
				return x.check(filepath.Join(rel, sub), dir, sub)
			}) {
				return explorerPlan{}, false
			}
		}
		return explorerPlan{
			action: "delete", args: map[string]string{"path": abs}, also: also,
			command: cmd + shellQuote(abs), description: "deleted in the explorer", result: rel,
			done: func() bool { _, err := os.Lstat(abs); return err != nil },
		}, true
	})
}

// explorerCall carries one request through its checks. A check that fails
// writes the reply itself and returns false.
type explorerCall struct {
	w   http.ResponseWriter
	v   *workspaceView
	pol *policy.Engine
}

func (s *Server) explorerOp(w http.ResponseWriter, r *http.Request, req any, plan func(*explorerCall) (explorerPlan, bool)) {
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

	live.manualMu.Lock()
	defer live.manualMu.Unlock()
	p, ok := plan(&explorerCall{w: w, v: v, pol: live.Loop.Policy})
	if !ok {
		return
	}
	// Detached from the request: a client that goes away must not stop an rm -r half way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), explorerTimeout)
	defer cancel()
	p.args["runs"] = p.command
	args, _ := json.Marshal(p.args)
	runArgs, _ := json.Marshal(map[string]string{"command": p.command, "description": p.description})
	also := make([]agent.Subject, 0, len(p.also)+len(p.removes))
	// Each spelling is judged once per action: on a path with no links both spellings match.
	seen := make(map[[2]string]bool, len(p.also)+len(p.removes))
	add := func(action string, paths []string) {
		for _, path := range paths {
			if seen[[2]string{action, path}] {
				continue
			}
			seen[[2]string{action, path}] = true
			a, _ := json.Marshal(map[string]string{"path": path})
			also = append(also, agent.Subject{Action: action, Args: a})
		}
	}
	add("", p.also)
	add("delete", p.removes)
	res, err := live.Loop.ManualAs(ctx, sess, p.action, "u"+newSessionID(), args, "bash", runArgs, also...)
	switch {
	case err != nil:
		WriteError(w, http.StatusInternalServerError, "the change could not be recorded, so it was not made")
	case res.IsError:
		WriteError(w, http.StatusForbidden, res.Content)
	case res.ExitCode != nil && *res.ExitCode != 0:
		WriteError(w, http.StatusConflict, strings.TrimSpace(res.Content))
	case !p.done():
		WriteError(w, http.StatusConflict, "the command ran but the change is not on disk; reload the explorer and try again")
	default:
		WriteJSON(w, http.StatusOK, explorerResponse{Path: filepath.ToSlash(p.result)})
	}
}

// named takes a path the client sent: relative, inside the workspace and not
// the workspace itself. It returns the path as named, links unresolved, so a
// link is renamed or removed, never its target.
func (x *explorerCall) named(p string) (string, string, bool) {
	local := filepath.Clean(filepath.FromSlash(p))
	if strings.TrimSpace(p) == "" || filepath.IsAbs(local) || !filepath.IsLocal(local) || local == "." {
		writeViewError(x.w, errNotInView)
		return "", "", false
	}
	return local, filepath.Join(x.v.sess.Root, local), true
}

func (x *explorerCall) present(abs string) (os.FileInfo, bool) {
	info, err := os.Lstat(abs)
	if err != nil {
		writeViewError(x.w, errNotInView)
		return nil, false
	}
	return info, true
}

func (x *explorerCall) absent(abs string) bool {
	if _, err := os.Lstat(abs); err == nil {
		WriteError(x.w, http.StatusConflict, "something with that name is already there")
		return false
	}
	return true
}

// check refuses a path the view would not show, or a save to which would be
// refused. The write rules are put to it as named and with its folder's links
// followed, which is where rm and mv act. inside names the entry within a
// folder being changed, for the reply.
func (x *explorerCall) check(rel string, dir bool, inside string) bool {
	code, why := x.judge(rel, dir)
	if code == 0 {
		return true
	}
	if inside != "" {
		if code != http.StatusForbidden {
			why = "the workbench does not show or change it"
		}
		code, why = http.StatusForbidden, "this folder holds "+filepath.ToSlash(inside)+": "+why
	}
	WriteError(x.w, code, why)
	return false
}

// subjects spells rel as a rule on the action may name it: as named and with its
// folder's links followed, and a folder also by what is inside it.
func (x *explorerCall) subjects(rel string, dir bool) []string {
	abs := filepath.Join(x.v.sess.Root, rel)
	var out []string
	for _, spelling := range []string{abs, filepath.Join(realPath(filepath.Dir(abs)), filepath.Base(abs))} {
		if len(out) > 0 && spelling == out[0] {
			continue
		}
		out = append(out, spelling)
		if dir {
			out = append(out, spelling+string(filepath.Separator))
		}
	}
	return out
}

func (x *explorerCall) judge(rel string, dir bool) (int, string) {
	if _, err := x.v.resolve(rel); err != nil {
		if errors.Is(err, errDenied) {
			return http.StatusForbidden, err.Error()
		}
		return http.StatusNotFound, errNotInView.Error()
	}
	abs := filepath.Join(x.v.sess.Root, rel)
	for _, spelling := range []string{abs, filepath.Join(realPath(filepath.Dir(abs)), filepath.Base(abs))} {
		subjects := []string{spelling}
		if dir {
			// A rule such as write(**/locked/**) names what is inside the folder.
			subjects = append(subjects, spelling+string(filepath.Separator))
		}
		for _, subject := range subjects {
			args, _ := json.Marshal(map[string]string{"path": subject})
			if d := x.pol.Evaluate("write", true, args); d.Decision == policy.Deny {
				return http.StatusForbidden, "Denied: " + d.Reason
			}
		}
	}
	return 0, ""
}

var (
	errTooMany = errors.New("too many entries")
	errRefused = errors.New("refused")
)

// contents calls fn for each entry under the folder rel, by its path within
// it, up to maxExplorerEntries; fn returning false ends the walk.
func (x *explorerCall) contents(rel string, fn func(sub string, dir bool) bool) bool {
	top := filepath.Join(x.v.sess.Root, rel)
	seen := 0
	err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == top {
			return nil
		}
		if seen++; seen > maxExplorerEntries {
			return errTooMany
		}
		sub, _ := filepath.Rel(top, p)
		if !fn(sub, d.IsDir()) {
			return errRefused
		}
		return nil
	})
	switch {
	case err == nil:
		return true
	case errors.Is(err, errRefused):
	case errors.Is(err, errTooMany):
		WriteError(x.w, http.StatusRequestEntityTooLarge, fmt.Sprintf("this folder holds more than %d entries; change it from the terminal", maxExplorerEntries))
	default:
		writeViewError(x.w, errNotInView)
	}
	return false
}

// moved reports whether the entry that was at from is now at to. mv -n
// succeeds without moving when something took the name first, and a folder
// that did would otherwise pass for the moved one.
func moved(before os.FileInfo, from, to string) bool {
	if _, err := os.Lstat(from); err == nil {
		return false
	}
	after, err := os.Lstat(to)
	return err == nil && os.SameFile(before, after)
}

// shellQuote makes one word of s for /bin/sh, whatever it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
