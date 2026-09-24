package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Changing what the agent can do, without a restart.
//
// Everything the agent reaches — its tools, its skills, its MCP servers, its
// index — was built once in serveCmd and frozen for the life of the process.
// Adding a skill meant editing JSON on the host and restarting, which is not
// something a person operating a deployment through a browser can do.
//
// Two rules shape the design:
//
//  1. **Admin only.** A skill is *instructions* and an MCP server is a *tool
//     surface*. Letting any signed-in user add either is letting them rewrite
//     what the agent does, which is privilege escalation with a settings page
//     for a face. Every route here is behind the admin group.
//
//  2. **Copy-on-write, never mutate in place.** A Registry is read on every
//     turn of every session from many goroutines. A change clones, mutates the
//     clone, and swaps the pointer under a write lock. Running sessions keep
//     the registry they started with, which is also correct: a tool set that
//     changed mid-session would mean the model was told about tools that were
//     not there when it planned.

// mutable holds the state a settings change can replace.
//
// Deliberately separate from Options, which stays immutable. Mixing "set once
// at construction" with "changes at runtime" in one struct is how a field ends
// up read without the lock.
type mutable struct {
	mu       sync.RWMutex
	registry *tools.Registry
	skills   *skills.Registry
	gateway  *mcp.Gateway
	// cfg is the live configuration. Copied out under the read lock rather
	// than shared, because config.Config is a value type read from request
	// handlers on every session creation.
	cfg config.Config
}

func (m *mutable) snapshot() (*tools.Registry, *skills.Registry, config.Config) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.registry, m.skills, m.cfg
}

func (m *mutable) toolRegistry() *tools.Registry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.registry
}

// swapTools installs a modified copy of the tool registry.
//
// The mutation runs against a clone while the lock is held, so no reader ever
// observes a half-built registry: they hold the old pointer until the swap, and
// the new one after it.
func (m *mutable) swapTools(mutate func(*tools.Registry)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.registry.Clone()
	mutate(next)
	m.registry = next
}

// ------------------------------------------------------------------ read

type settingsView struct {
	Skills    []skillView    `json:"skills"`
	MCP       []mcpView      `json:"mcp"`
	Tools     []string       `json:"tools"`
	Retrieval retrievalView  `json:"retrieval"`
	Providers []providerInfo `json:"providers"`
	// Managed reports that org policy owns this config, so changes made here
	// would be silently overridden on restart. Saying so beats letting an
	// operator watch a setting revert and wonder why.
	Managed bool `json:"managed"`
}

type skillView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	HasPipeline bool   `json:"has_pipeline"`
}

type mcpView struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type retrievalView struct {
	Enabled bool `json:"enabled"`
	Docs    int  `json:"docs,omitempty"`
	Terms   int  `json:"terms,omitempty"`
}

// getSettings reports what the agent can currently reach.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	reg, sk, cfg := s.state.snapshot()

	v := settingsView{
		Providers: s.providers(),
		Managed:   cfg.Managed,
		Retrieval: retrievalView{Enabled: cfg.Retrieval.Enabled},
	}
	if reg != nil {
		v.Tools = reg.Names()
	}
	if sk != nil {
		for _, one := range sk.All() {
			v.Skills = append(v.Skills, skillView{
				Name:        one.Name,
				Description: one.Description,
				HasPipeline: one.Pipeline != nil,
			})
		}
	}
	sort.Slice(v.Skills, func(i, j int) bool { return v.Skills[i].Name < v.Skills[j].Name })

	s.state.mu.RLock()
	gw := s.state.gateway
	s.state.mu.RUnlock()
	if gw != nil {
		for _, line := range gw.Status() {
			v.MCP = append(v.MCP, mcpView{Name: line, Status: "connected"})
		}
	}
	if v.Skills == nil {
		v.Skills = []skillView{}
	}
	if v.MCP == nil {
		v.MCP = []mcpView{}
	}
	WriteJSON(w, http.StatusOK, v)
}

// ----------------------------------------------------------------- skills

// reloadSkills rescans the skill directories.
//
// Skills are mounted read-only into the container on purpose: the agent must
// not be able to write its own operating instructions. So this rescans what an
// operator put there; it does not accept a skill body over HTTP.
func (s *Server) reloadSkills(w http.ResponseWriter, r *http.Request) {
	// The ROOTS, not the individual skill directories: Load scans a root for
	// subdirectories, so scanning inside a skill finds nothing at all.
	dirs := s.opts.SkillRoots
	if len(dirs) == 0 {
		WriteError(w, http.StatusNotImplemented, "no skill directories are configured")
		return
	}

	reg, errs := skills.Load(dirs)
	s.state.mu.Lock()
	s.state.skills = reg
	s.state.mu.Unlock()

	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	s.adminAudit(r, "skills.reloaded", "", map[string]any{
		"count": reg.Len(), "errors": len(errs)})
	WriteJSON(w, http.StatusOK, map[string]any{
		"loaded":   reg.Len(),
		"warnings": msgs,
	})
}

// -------------------------------------------------------------------- mcp

type mcpRequest struct {
	Name    string   `json:"name"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
}

// addMCP connects an MCP server and exposes its tools.
//
// This is the sharpest edge in the settings surface: an MCP server is a tool
// surface the agent will call, so adding one is granting a capability. It is
// admin-gated for that reason, and the name is validated by the gateway before
// anything is spawned.
func (s *Server) addMCP(w http.ResponseWriter, r *http.Request) {
	var req mcpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if (req.Command == "") == (req.URL == "") {
		WriteError(w, http.StatusBadRequest,
			"give exactly one of command or url")
		return
	}

	s.state.mu.RLock()
	gw := s.state.gateway
	s.state.mu.RUnlock()
	if gw == nil {
		WriteError(w, http.StatusNotImplemented, "MCP is not available here")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	before := map[string]bool{}
	for _, t := range gw.Tools() {
		before[t.Name()] = true
	}

	if errs := gw.Connect(ctx, []mcp.ServerConfig{{
		Name: req.Name, Command: req.Command, Args: req.Args,
		URL: req.URL, Enabled: true,
	}}); len(errs) > 0 {
		WriteError(w, http.StatusBadRequest, errs[0].Error())
		return
	}

	// Only the tools this server brought are added, so a reconnect of one
	// server does not disturb another's.
	added := []string{}
	s.state.swapTools(func(reg *tools.Registry) {
		for _, t := range gw.Tools() {
			if !before[t.Name()] {
				reg.Add(t)
				added = append(added, t.Name())
			}
		}
	})

	s.adminAudit(r, "mcp.added", req.Name, map[string]any{
		"tools": added, "command": req.Command, "url": req.URL})
	WriteJSON(w, http.StatusOK, map[string]any{
		"name":  req.Name,
		"tools": added,
	})
}

// --------------------------------------------------------------- retrieval

// reindex rebuilds the code index.
//
// Runs in the background: a monorepo takes long enough that holding the request
// open would time out, and the caller only needs to know it started.
func (s *Server) reindex(w http.ResponseWriter, r *http.Request) {
	if s.opts.Index == nil {
		WriteError(w, http.StatusNotImplemented,
			"retrieval is not enabled on this deployment")
		return
	}
	user := UserOf(r.Context())
	s.adminAudit(r, "index.rebuild_started", "", nil)
	go func() {
		// Detached from the request on purpose: the client disconnecting must
		// not abandon a half-built index.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := s.opts.Index.Build(ctx, s.opts.IndexOptions); err != nil {
			s.log.Error("reindex failed", "error", err, "by", user)
			return
		}
		s.log.Info("reindex complete", "by", user)
	}()
	WriteJSON(w, http.StatusAccepted, map[string]string{"status": "reindexing"})
}

// skillListing renders the skill index for the system prompt.
//
// Computed per session rather than taken from the string built at startup, so a
// skill added through settings reaches the next session's prompt. Options
// .SkillListing remains the fallback for embedders that inject a rendered
// listing and no registry.
func (s *Server) skillListing(reg *skills.Registry) string {
	if reg != nil {
		return reg.Listing()
	}
	return s.opts.SkillListing
}
