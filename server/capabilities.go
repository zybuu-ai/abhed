package server

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

//go:embed ide.html
var ideHTML string

// The editor and terminal components are built into the binary, under their
// own licences (ide/vendor/NOTICE), so the page still loads nothing from anywhere.
//
//go:embed ide/vendor
var ideVendor embed.FS

var vendorETags sync.Map

// vendorETag hashes an embedded file once; the content never changes at run time.
func vendorETag(name string, data []byte) string {
	if v, ok := vendorETags.Load(name); ok {
		return v.(string)
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(data))
	vendorETags.Store(name, etag)
	return etag
}

func (s *Server) serveIDEVendor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	data, err := ideVendor.ReadFile("ide/vendor/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Revalidated on every load, so an upgrade never runs a stale editor
	// against a new page; the ETag makes the check a 304.
	etag := vendorETag(name, data)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(data)
}

// serveIDE serves the workbench: the agent beside the code it is changing.
// The same strict policy as the console — nothing loads from anywhere, so it
// opens on an air-gapped network.
func (s *Server) serveIDE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write([]byte(ideHTML))
}

// capabilities is what the agent on this server can reach: its tools, skills,
// MCP servers and extensions, and the policy and sandbox that bound them. The
// admin settings page shows the same things to the person who can change
// them; this shows them to the person about to rely on them.
type capabilities struct {
	Model       capModel       `json:"model"`
	Sandbox     capSandbox     `json:"sandbox"`
	Permissions capPermissions `json:"permissions"`
	Tools       []capTool      `json:"tools"`
	Skills      []skillView    `json:"skills"`
	MCP         []mcpView      `json:"mcp"`
	Extensions  []capExtension `json:"extensions"`
}

type capModel struct {
	Name   string `json:"name"`
	Window int    `json:"context_window"`
}

type capSandbox struct {
	Tier    string `json:"tier"`
	Network bool   `json:"network"`
}

type capPermissions struct {
	Mode    string   `json:"mode"`
	Deny    []string `json:"deny"`
	Ask     []string `json:"ask"`
	Allow   []string `json:"allow"`
	Managed bool     `json:"managed"`
}

type capTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Mutates is what the policy keys on: a mutating tool asks before it
	// runs unless a rule or the mode says otherwise.
	Mutates bool `json:"mutates"`
	// Source is builtin, mcp or skill; Server names the MCP server.
	Source string `json:"source"`
	Server string `json:"server,omitempty"`
}

type capExtension struct {
	Name   string   `json:"name"`
	Events []string `json:"events"`
}

func (s *Server) getCapabilities(w http.ResponseWriter, _ *http.Request) {
	reg, sk, cfg := s.state.snapshot()
	prof := s.opts.Adapter.Profile()
	c := capabilities{
		Model:   capModel{Name: prof.Name, Window: prof.ContextWindow},
		Sandbox: capSandbox{Tier: orDefaultStr(cfg.Sandbox.MinTier, "process"), Network: cfg.Sandbox.AllowNetwork},
		Permissions: capPermissions{
			Mode: orDefaultStr(cfg.Permissions.Mode, "default"), Managed: cfg.Managed,
			Deny: nonNil(cfg.Permissions.Deny), Ask: nonNil(cfg.Permissions.Ask), Allow: nonNil(cfg.Permissions.Allow),
		},
		Tools: []capTool{}, Skills: []skillView{}, MCP: []mcpView{}, Extensions: []capExtension{},
	}
	if reg != nil {
		for _, t := range reg.All() {
			ct := capTool{Name: t.Name(), Description: firstSentence(t.Description()), Mutates: t.Mutates(), Source: "builtin"}
			if rest, ok := strings.CutPrefix(t.Name(), "mcp__"); ok {
				ct.Source = "mcp"
				ct.Server, _, _ = strings.Cut(rest, "__")
			} else if t.Name() == "skill" {
				ct.Source = "skill"
			}
			c.Tools = append(c.Tools, ct)
		}
	}
	// recall is bound to one session's record, so the loop adds it to its own
	// copy of the registry and the shared one never holds it. The agent has
	// it all the same, and a list that left it out would be wrong.
	if s.store != nil {
		c.Tools = append(c.Tools, capTool{Name: "recall", Source: "builtin",
			Description: "Read this session's own record, to get back text that has left the context window."})
	}
	if sk != nil {
		for _, one := range sk.All() {
			c.Skills = append(c.Skills, skillView{Name: one.Name, Description: one.Description, HasPipeline: one.Pipeline != nil})
		}
		sort.Slice(c.Skills, func(i, j int) bool { return c.Skills[i].Name < c.Skills[j].Name })
	}
	s.state.mu.RLock()
	gw := s.state.gateway
	s.state.mu.RUnlock()
	if gw != nil {
		for _, line := range gw.Status() {
			c.MCP = append(c.MCP, mcpView{Name: line, Status: "connected"})
		}
	}
	// Names and events only. An extension's command line and environment are
	// the operator's business and may carry credentials.
	for _, e := range cfg.Extensions {
		c.Extensions = append(c.Extensions, capExtension{Name: e.Name, Events: nonNil(e.Events)})
	}
	WriteJSON(w, http.StatusOK, c)
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// firstSentence keeps a tool's description to what fits in a list row. The
// full text is written for the model and can run to a paragraph.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 && i < 220 {
		return s[:i+1]
	}
	if len(s) > 220 {
		return clipUTF8(s, 220) + "…"
	}
	return s
}

func clipUTF8(s string, n int) string {
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
