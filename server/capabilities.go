package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/tools"
)

//go:embed ide.html
var ideHTML string

var idePage = brandify(ideHTML)

// The editor and terminal components are built into the binary, under their
// own licences (ide/vendor/NOTICE), so the page still loads nothing from anywhere.
// They are stored gzipped, as sent to nearly every browser; the rare client
// that does not take gzip gets them unpacked once and kept.
//
//go:embed ide/vendor/*.gz ide/vendor/NOTICE
var ideVendor embed.FS

// vendorFile is one component in both encodings, each with its own ETag.
type vendorFile struct {
	gz       []byte
	gzTag    string
	once     sync.Once
	plain    []byte
	plainTag string
	err      error
}

var vendorFiles sync.Map

func etagOf(data []byte, suffix string) string {
	return fmt.Sprintf(`"%x%s"`, sha256.Sum256(data), suffix)
}

// vendorAsset finds a component by the name the page asks for.
func vendorAsset(name string) (*vendorFile, bool) {
	if f, ok := vendorFiles.Load(name); ok {
		return f.(*vendorFile), true
	}
	f := &vendorFile{}
	if gz, err := ideVendor.ReadFile("ide/vendor/" + name + ".gz"); err == nil {
		f.gz, f.gzTag = gz, etagOf(gz, "-gz")
	} else if name == "NOTICE" {
		plain, err := ideVendor.ReadFile("ide/vendor/NOTICE")
		if err != nil {
			return nil, false
		}
		f.once.Do(func() { f.plain, f.plainTag = plain, etagOf(plain, "") })
	} else {
		return nil, false
	}
	got, _ := vendorFiles.LoadOrStore(name, f)
	return got.(*vendorFile), true
}

// unpacked returns the component without its gzip encoding.
func (f *vendorFile) unpacked() ([]byte, string, error) {
	f.once.Do(func() {
		zr, err := gzip.NewReader(bytes.NewReader(f.gz))
		if err != nil {
			f.err = err
			return
		}
		f.plain, f.err = io.ReadAll(zr)
		f.plainTag = etagOf(f.plain, "")
	})
	return f.plain, f.plainTag, f.err
}

// acceptsGzip reads Accept-Encoding for gzip with a non-zero weight.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		q := strings.ReplaceAll(strings.TrimSpace(params), " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}

func (s *Server) serveIDEVendor(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	f, ok := vendorAsset(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	switch {
	case strings.HasSuffix(name, ".js"):
		h.Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		h.Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".ttf"):
		h.Set("Content-Type", "font/ttf")
	default:
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	if strings.HasSuffix(name, ".worker.js") {
		// A worker takes its policy from its own response, not the page's. These
		// parse untrusted file content, so they may load nothing and call nowhere.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'")
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Vary", "Accept-Encoding")
	// Revalidated on every load, so an upgrade never runs a stale editor
	// against a new page; the ETag makes the check a 304.
	h.Set("Cache-Control", "private, no-cache")
	var body []byte
	var etag string
	if f.gz != nil && acceptsGzip(r) {
		body, etag = f.gz, f.gzTag
		h.Set("Content-Encoding", "gzip")
	} else {
		plain, tag, err := f.unpacked()
		if err != nil {
			http.Error(w, "the component is damaged", http.StatusInternalServerError)
			return
		}
		body, etag = plain, tag
	}
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// serveIDE serves the workbench: the agent beside the code it is changing.
// The same strict policy as the console — nothing loads from anywhere, so it
// opens on an air-gapped network. font-src admits the editor's own icon font;
// its workers are same-origin scripts, which script-src already covers.
func (s *Server) serveIDE(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	_, _ = w.Write([]byte(idePage))
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
	// Backend is the mechanism in force, when the server knows it.
	Backend string `json:"backend,omitempty"`
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
	// The tier the sandbox actually provides, which may be stronger than the minimum.
	if t, ok := bashTool(reg); ok {
		if b, ok := t.(tools.Bash); ok && b.Isolation.Tier != "" {
			c.Sandbox.Tier, c.Sandbox.Backend = b.Isolation.Tier, b.Isolation.Backend
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

func bashTool(reg *tools.Registry) (tools.Tool, bool) {
	if reg == nil {
		return nil, false
	}
	return reg.Get("bash")
}
