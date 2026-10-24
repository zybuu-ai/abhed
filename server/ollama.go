package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ollama"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// A local Ollama model chosen in the workbench or the console.
//
// The picker asks GET /v1/ollama/status for a provider that Ollama serves
// and shows a card: what is wrong, the numbered steps a person can run, and
// the next step when this person may take it. Starting, installing and
// pulling act on the server's host, so only an administrator may (or,
// where nobody signs in, a request from the host itself); everyone else
// gets the message and the steps. The card asks before every action; the
// server applies model.ollama.assist, acts only on the host's own loopback
// Ollama, and records each attempt in the session the request names, as
// well as in the server's log and audit.

//go:embed ollama_card.js
var ollamaCardJS string

// withOllamaCard puts the card's script where a page marks it.
func withOllamaCard(page string) string {
	return strings.Replace(page, "/*OLLAMA-CARD*/", ollamaCardJS, 1)
}

// ollamaEnv is the host the assist acts on; tests replace it.
var ollamaEnv = ollama.DefaultEnv

// ollamaRequest is the body of the actions.
type ollamaRequest struct {
	Provider string `json:"provider"`
	// SessionID is the session whose record takes the attempt, when the
	// picker belongs to one.
	SessionID string `json:"session_id,omitempty"`
	// Always, on start, is "always start without asking".
	Always bool `json:"always,omitempty"`
}

// ollamaAdmin reports whether the caller may act on this host: an
// administrator, or, where nobody signs in, a request from the host itself.
func (s *Server) ollamaAdmin(r *http.Request) bool {
	if s.IsAdmin(r) {
		return true
	}
	mw := s.authMiddleware()
	if len(mw.Providers) > 0 || mw.Verifier != nil || mw.TrustHeaders {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ollamaAssist resolves the provider and the assist for this caller, or
// writes why not and returns false.
func (s *Server) ollamaAssist(w http.ResponseWriter, r *http.Request, req ollamaRequest) (ollama.Assist, ollama.Target, bool) {
	cfg := s.opts.Config
	if req.Provider == "" || cfg.ModelAllowed(req.Provider) != nil {
		WriteError(w, http.StatusBadRequest, errUnknownProvider.Error())
		return ollama.Assist{}, ollama.Target{}, false
	}
	t, ok := toolset.OllamaTarget(cfg, req.Provider)
	if !ok {
		WriteError(w, http.StatusBadRequest, "that provider is not served by Ollama")
		return ollama.Assist{}, ollama.Target{}, false
	}
	user := UserOf(r.Context())
	a := toolset.OllamaAssist(cfg, ollamaEnv(), "server", s.ollamaAdmin(r), sandbox.InAgentCommand(), func(typ agent.EventType, ev agent.OllamaAction) {
		ev.By = agent.ByUser
		s.log.Info("ollama "+ev.Op, "outcome", ev.Outcome, "provider", ev.Provider, "model", ev.Model, "user", user, "problem", ev.Problem)
		s.adminAudit(r, "ollama."+ev.Op, ev.Provider, map[string]any{"outcome": ev.Outcome, "model": ev.Model, "command": ev.Command, "problem": ev.Problem})
		if req.SessionID != "" {
			if err := s.recordPerson(context.WithoutCancel(r.Context()), req.SessionID, typ, ev); err != nil {
				s.log.Warn("ollama attempt not in the session's record", "session", req.SessionID, "error", err)
			}
		}
	})
	return a, t, true
}

// recordPerson records a person's own action in a live session of theirs,
// between turns, as a rename is.
func (s *Server) recordPerson(ctx context.Context, id string, typ agent.EventType, payload any) error {
	if !validSessionID(id) {
		return errNoSession
	}
	live, found := s.session(ctx, id, TenantOf(ctx), UserOf(ctx))
	if !found {
		return errNoSession
	}
	live.claimMu.Lock()
	defer live.claimMu.Unlock()
	newly, err := s.claimLocked(ctx, id, live)
	if err != nil {
		return err
	}
	if newly {
		s.holdClaim(id, live)
	}
	live.mu.Lock()
	busy := live.State == "running" || live.State == "waiting_approval"
	live.mu.Unlock()
	if busy {
		return errors.New("the session is mid-turn")
	}
	_, err = live.Loop.Recorder.Record(typ, agent.ActorUser, agent.Trusted, payload)
	return err
}

func decodeOllama(w http.ResponseWriter, r *http.Request) (ollamaRequest, bool) {
	var req ollamaRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		badBody(w, err, "invalid request")
		return req, false
	}
	return req, true
}

// ollamaFailure answers a Problem: 403 when this caller or the
// configuration may not, 409 for a host that is not ready, 502 when the
// step failed.
func ollamaFailure(w http.ResponseWriter, p *ollama.Problem, st ollama.Status) {
	code := http.StatusBadGateway
	switch p.Kind {
	case ollama.KindNotAdmin, ollama.KindAssistOff, ollama.KindInAgent, ollama.KindRemote:
		code = http.StatusForbidden
	case ollama.KindNotInstalled, ollama.KindNeedsTerminal, ollama.KindManual, ollama.KindDiskShort, ollama.KindPortBusy, ollama.KindModelNotFound:
		code = http.StatusConflict
	case ollama.KindCancelled:
		code = http.StatusRequestTimeout
	}
	WriteJSON(w, code, map[string]any{"error": p.Message, "problem": p, "status": st})
}

// getOllamaStatus is GET /v1/ollama/status?provider=NAME: the card's content.
func (s *Server) getOllamaStatus(w http.ResponseWriter, r *http.Request) {
	a, t, ok := s.ollamaAssist(w, r, ollamaRequest{Provider: r.URL.Query().Get("provider")})
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, a.Report(r.Context(), t))
}

// postOllamaStart is POST /v1/ollama/start.
func (s *Server) postOllamaStart(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeOllama(w, r)
	if !ok {
		return
	}
	a, t, ok := s.ollamaAssist(w, r, req)
	if !ok {
		return
	}
	st := a.Env.Check(r.Context(), t)
	if st.Running {
		WriteJSON(w, http.StatusOK, map[string]any{"status": st})
		return
	}
	consent := "asked"
	if req.Always && a.May(ollama.OpStart, t, st) == nil {
		if err := a.Env.SetStartAlways(true); err != nil {
			s.log.Warn("the choice to always start Ollama was not kept", "error", err)
		}
	} else if !req.Always && a.Env.StartAlways() {
		consent = "always"
	}
	started, p := a.Start(r.Context(), t, st, consent)
	if p != nil {
		ollamaFailure(w, p, a.Env.Check(r.Context(), t))
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"started": started, "status": a.Env.Check(r.Context(), t)})
}

// postOllamaInstall is POST /v1/ollama/install. The Linux install script is
// never run by the server: it may ask for a password, which only the
// person at the host's terminal can give.
func (s *Server) postOllamaInstall(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeOllama(w, r)
	if !ok {
		return
	}
	a, t, ok := s.ollamaAssist(w, r, req)
	if !ok {
		return
	}
	st := a.Env.Check(r.Context(), t)
	if st.Installed || st.Running {
		WriteJSON(w, http.StatusOK, map[string]any{"status": st})
		return
	}
	plan := a.Env.PlanInstall()
	var buf tailWriter
	if p := a.Install(r.Context(), t, st, plan, false, nil, &buf, &buf); p != nil {
		ollamaFailure(w, p, a.Env.Check(r.Context(), t))
		return
	}
	outcome := agent.OllamaOK
	if plan.Kind == ollama.InstallOpen {
		outcome = agent.OllamaManual
	}
	WriteJSON(w, http.StatusOK, map[string]any{"outcome": outcome, "output": buf.String(), "status": a.Env.Check(r.Context(), t)})
}

// postOllamaPull is POST /v1/ollama/pull: a stream of JSON lines, each
// {"progress": {...}}, then {"status": {...}} or {"problem": {...}}. Closing
// the request stops the download; Ollama keeps what it fetched.
func (s *Server) postOllamaPull(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeOllama(w, r)
	if !ok {
		return
	}
	a, t, ok := s.ollamaAssist(w, r, req)
	if !ok {
		return
	}
	st := a.Env.Check(r.Context(), t)
	if st.Pulled {
		WriteJSON(w, http.StatusOK, map[string]any{"status": st})
		return
	}
	if p := a.May(ollama.OpPull, t, st); p != nil {
		a.Note(ollama.OpPull, agent.OllamaRefused, t, p)
		ollamaFailure(w, p, st)
		return
	}
	fit, p := a.Env.CheckFit(r.Context(), t, st)
	if p != nil {
		a.Note(ollama.OpPull, agent.OllamaFailed, t, p)
		ollamaFailure(w, p, st)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	fl, _ := w.(http.Flusher)
	send := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(v)
		if fl != nil {
			fl.Flush()
		}
	}
	p = a.Pull(r.Context(), t, st, fit, func(pg ollama.Progress) { send(map[string]any{"progress": pg}) })
	if p != nil {
		send(map[string]any{"problem": p})
		return
	}
	send(map[string]any{"status": a.Env.Check(context.WithoutCancel(r.Context()), t)})
}

// postOllamaForget is POST /v1/ollama/forget: take back "always start".
func (s *Server) postOllamaForget(w http.ResponseWriter, r *http.Request) {
	if !s.ollamaAdmin(r) {
		WriteError(w, http.StatusForbidden, "only an administrator of this server can change that")
		return
	}
	if err := ollamaEnv().SetStartAlways(false); err != nil {
		WriteError(w, http.StatusInternalServerError, "the choice could not be forgotten")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tailWriter keeps the last of an install's output.
type tailWriter struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
