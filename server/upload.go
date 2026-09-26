package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Uploading a document into a session.
//
// A deep agent gets handed files — a design doc, a spec, an incident report —
// and asking someone to first copy it into the workspace defeats the point of
// a chat interface.
//
// The uploaded file lands in a per-session directory INSIDE the workspace, so
// the existing scoping rules cover it with no special case: the agent reads it
// with the ordinary `read` tool, subject to the same boundary as any other
// file.
//
// For text and documents there is deliberately no separate "attachment"
// pathway into the model's context, because that would be a second way for
// untrusted bytes to reach the prompt, and the trust model depends on there
// being one.
//
// Images are the single, deliberate exception. They cannot be read as text at
// all, so the choice is between a second pathway and no vision — and a stated
// invariant is worth overturning openly rather than working around. The
// exception is kept narrow: a recognised image format identified by MAGIC
// BYTES rather than by the filename the uploader chose, still written into the
// same per-session directory, and still tagged untrusted like every other
// observation. An image reaching a model that cannot see is refused loudly
// rather than dropped (model.CheckVision).

// maxUploadBytes bounds a single file. Large enough for a real specification,
// small enough that a session directory cannot fill the disk.
const maxUploadBytes = 32 << 20 // 32 MiB

// uploadDirName is the workspace-relative directory that holds attachments.
// It is deliberately not a dot-directory: a hidden name invites exactly the
// kind of blanket deny rule that broke uploads, and there is nothing secret
// about a file the user attached on purpose.
const uploadDirName = "uploads"

// uploadResponse tells the caller where the file landed, so the UI can name
// the path in the message it sends and the model can read it.
type uploadResponse struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	Kind      string `json:"kind,omitempty"`
	Extracted bool   `json:"extracted"`
	Preview   string `json:"preview,omitempty"`
	Note      string `json:"note,omitempty"`
	// Image and MediaType mark a file the console should attach as content
	// rather than only naming by path.
	Image     bool   `json:"image,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

// uploadFile accepts a multipart file and stores it under the session's
// upload directory.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	// An empty id means the file was attached before the chat existed, which
	// is the normal case in a new conversation: someone drops in a document
	// and then types the question about it. Staging it under a generated
	// directory avoids the alternative — creating a throwaway session to hold
	// the file, which produced a real bug: the UI opened its event stream on
	// the placeholder turn, that turn ended, and the follow-up message's
	// events had nowhere to render.
	sessionID := r.PathValue("id")
	staged := sessionID == "" || sessionID == "new"
	if staged {
		var b [6]byte
		rand.Read(b[:])
		sessionID = "staged-" + hex.EncodeToString(b[:])
	}
	// The ID becomes a directory name below. filepath.Join would CLEAN a "..",
	// not reject it — resolving the traversal rather than stopping it — so the
	// segment is validated before it is ever joined.
	if !validSessionID(sessionID) {
		WriteError(w, http.StatusBadRequest, "invalid session id")
		return
	}
	// A caller-supplied id must belong to the caller. The staged case above
	// generated its own id, so it is always the caller's; every other id came
	// from the path and has to be checked, exactly as download, stream and
	// delete do. Without this any authenticated account could POST a file into
	// another session's upload directory by naming that session's id — content
	// the victim's agent then reads as (untrusted) input, a cross-session
	// prompt-injection delivery channel. Failing closed (404, matching the
	// sibling routes) also refuses to confirm whether an id exists.
	if !staged && !s.mayAccess(r, sessionID) {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}

	// Reject oversize bodies before reading them into memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		WriteError(w, http.StatusBadRequest,
			fmt.Sprintf("could not read the upload (limit %d MiB): %v",
				maxUploadBytes>>20, err))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "no file in the request")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "read upload: "+err.Error())
		return
	}
	if len(data) > maxUploadBytes {
		WriteError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("file is larger than the %d MiB limit", maxUploadBytes>>20))
		return
	}

	name := safeUploadName(header.Filename)
	// Uploads live in their own top-level directory, NOT under .abhed/.
	//
	// They were written to <workspace>/.abhed/uploads/<session> until a
	// deployment whose policy denied read(**/.abhed/**) — the ordinary rule
	// that keeps the agent out of Abhed's own config and users.json — refused
	// every file the user attached. The agent reported "denied by rule" for a
	// document the user had just handed it deliberately, which is exactly
	// backwards: .abhed/ holds things the agent must not read, and an upload
	// is the opposite of that.
	//
	// Keeping them inside the workspace preserves the property the rest of
	// this file depends on: the file is read with the ordinary read tool,
	// under the same boundary as any other workspace file, with no special
	// case in the policy engine.
	dir := filepath.Join(s.opts.Workspace, uploadDirName, sessionID)
	dest := filepath.Join(dir, name)
	// The upload folder is in the workspace, where the agent can plant a
	// link: a write through one could replace Abhed's state or leave it. The
	// file is created new under the workspace held open as a root, so no
	// link, even one swapped in now, leads it out or into the state.
	if err := s.createUpload(dest, data); err != nil {
		if errors.Is(err, tools.ErrState) || errors.Is(err, tools.ErrOutside) {
			WriteError(w, http.StatusForbidden, "the upload folder leads outside the workspace or into Abhed's state; remove the link in "+uploadDirName)
			return
		}
		WriteError(w, http.StatusInternalServerError, "save upload: "+err.Error())
		return
	}

	resp := uploadResponse{Path: dest, Name: name, Bytes: int64(len(data))}

	// Report now whether the agent will be able to read this, rather than
	// letting the model discover it mid-turn and improvise. A PDF of scanned
	// pages is the common case, and "I could not read it" is far more useful
	// before the question is asked than after.
	if kind := tools.DetectKind(data, name); kind != tools.KindPlain {
		resp.Kind = string(kind)
		text, err := tools.ExtractText(data, kind)
		if err != nil {
			resp.Note = err.Error()
		} else {
			resp.Extracted = true
			resp.Preview = firstLines(text, 3)
		}
	} else if mt := imageMediaType(data); mt != "" {
		// An image is the one binary the agent can now use directly, on a
		// vision-capable model.
		//
		// This deliberately widens the invariant stated at the top of this
		// file: image bytes reach the model as content rather than only as a
		// path for the read tool. That is a real change to the trust surface,
		// so it is narrow — a recognised image format, by magic bytes rather
		// than by the filename the uploader chose, and the bytes stay tagged
		// untrusted exactly as any other observation is.
		resp.Kind = "image"
		resp.Image = true
		resp.MediaType = mt
		resp.Note = "an image; readable by a vision-capable model"
	} else if isProbablyBinary(data) {
		resp.Note = "this looks like a binary file; the agent will not be able to read it as text"
	}

	WriteJSON(w, http.StatusOK, resp)
}

// safeUploadName reduces a client-supplied filename to something that cannot
// escape the upload directory. The name arrives from a browser and is not
// trustworthy: "../../.ssh/authorized_keys" is a legal multipart filename.
func safeUploadName(raw string) string {
	name := filepath.Base(strings.ReplaceAll(raw, "\\", "/"))
	name = strings.TrimSpace(name)
	// Base() maps these to themselves, so they need naming explicitly.
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		name = "upload"
	}
	// Keep it recognisable but inert: no control characters, no path
	// separators, no leading dot that would hide it.
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 32 || r == 127:
			return -1
		case r == '/' || r == '\\' || r == ':':
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "upload"
	}
	if len(name) > 120 {
		// Preserve the extension, which is what a person recognises.
		ext := filepath.Ext(name)
		name = name[:120-len(ext)] + ext
	}
	// A short suffix keeps two uploads of "report.pdf" from overwriting each
	// other, which would silently change what the agent reads.
	var b [4]byte
	rand.Read(b[:])
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + "-" + hex.EncodeToString(b[:]) + ext
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	out := strings.Join(lines, "\n")
	if len(out) > 400 {
		out = out[:400] + "…"
	}
	return out
}

// imageMediaType identifies an image by its magic bytes, returning "" for
// anything else.
//
// By content, never by extension: the filename comes from the uploader, so
// trusting ".png" would let anything at all be presented to the model as an
// image. The four formats here are the ones every vision-capable provider
// accepts; anything more exotic is better refused than half-supported.
func imageMediaType(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// isProbablyBinary mirrors the read tool's judgement so the two agree about
// what counts as readable.
func isProbablyBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

// createUpload writes a new upload under the workspace root.
func (s *Server) createUpload(dest string, data []byte) error {
	root, err := filepath.EvalSymlinks(s.opts.Workspace)
	if err != nil {
		return err
	}
	c, err := tools.NewStateSet(s.opts.Workspace).Confine(root, s.opts.Workspace)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	return c.CreateNew(dest, data, 0o600)
}
