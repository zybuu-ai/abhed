package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// uploadInto puts an uploaded file into a workspace folder for the person at
// the workbench. It is their write: the folder and the new name are held to
// the view's rules, the write rules are put to both spellings of the path as
// an explorer change's are, and the call and its result are recorded as theirs.
// The file is created new; a name already taken is refused, never replaced.
func (s *Server) uploadInto(w http.ResponseWriter, r *http.Request, dir, rawName string, data []byte) {
	live, _, ok := s.manualSession(w, r)
	if !ok {
		return
	}
	// A server whose sessions cannot write files gives the person no write by upload either.
	if _, found := live.Loop.Tools.Get("write"); !found {
		WriteError(w, http.StatusForbidden, "files cannot be written on this server, so nothing can be uploaded into a folder")
		return
	}
	v, err := s.openView()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "workspace is not readable")
		return
	}
	defer v.Close()

	folder := "."
	if dir != "" && dir != "." {
		if folder, err = v.resolve(dir); err != nil {
			writeViewError(w, err)
			return
		}
	}
	if info, err := os.Stat(filepath.Join(v.sess.Root, folder)); err != nil || !info.IsDir() {
		WriteError(w, http.StatusNotFound, "that folder is not there")
		return
	}
	name := cleanUploadName(rawName)
	rel, err := v.resolve(filepath.Join(folder, name))
	if err != nil {
		writeViewError(w, err)
		return
	}
	abs := filepath.Join(v.sess.Root, rel)
	if _, err := os.Lstat(abs); err == nil {
		WriteError(w, http.StatusConflict, "something named "+name+" is already in that folder")
		return
	}

	live.manualMu.Lock()
	defer live.manualMu.Unlock()
	callID := "u" + newSessionID()
	args, _ := json.Marshal(map[string]any{"path": abs, "bytes": len(data), "sha256": contentHash(data), "via": "upload"})
	// The folder as named, and with its links followed, is where the file lands: a
	// write rule on either spelling holds too.
	if d := writeDenial(live.Loop.Policy, abs, filepath.Join(realPath(filepath.Dir(abs)), name),
		filepath.Join(v.typed(dir), name)); d != nil {
		if err := live.Loop.ManualRefused("write", callID, args, *d); err != nil {
			writeUnrecorded(w, err, "the upload could not be recorded, so it was not made")
			return
		}
		WriteError(w, http.StatusForbidden, "Denied: "+d.Reason)
		return
	}
	refused, err := live.Loop.ManualCheck("write", callID, args)
	if err != nil {
		writeUnrecorded(w, err, "the upload could not be recorded, so it was not made")
		return
	}
	if refused != nil {
		WriteError(w, http.StatusForbidden, refused.Content)
		return
	}

	start := time.Now()
	werr := s.createUpload(abs, data)
	res := tools.Result{Content: fmt.Sprintf("uploaded %d bytes to %s", len(data), filepath.ToSlash(rel))}
	if werr != nil {
		res = tools.Result{Content: "upload not written: " + werr.Error(), IsError: true}
	}
	if err := live.Loop.ManualObserve(callID, "write", res, time.Since(start)); err != nil {
		s.log.Warn("an upload's result was not recorded", "session", r.PathValue("id"), "error", err)
	}
	switch {
	case errors.Is(werr, tools.ErrState) || errors.Is(werr, tools.ErrOutside):
		WriteError(w, http.StatusForbidden, "that folder leads outside the workspace or into Abhed's state")
		return
	case errors.Is(werr, fs.ErrExist):
		WriteError(w, http.StatusConflict, "something named "+name+" is already in that folder")
		return
	case werr != nil:
		WriteError(w, http.StatusInternalServerError, "the upload was not saved")
		return
	}
	resp := uploadResponse{Path: filepath.ToSlash(rel), Name: name, Bytes: int64(len(data))}
	describeUpload(&resp, data, name)
	WriteJSON(w, http.StatusOK, resp)
}
