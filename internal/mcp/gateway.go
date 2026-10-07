package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ServerConfig describes a registered MCP server.
//
// A server not in the registry does not run. Discovery does not imply trust,
// which is the whole point of an enterprise gateway (docs §03 §5).
type ServerConfig struct {
	Name string `json:"name"`
	// Command runs the server as a subprocess (stdio transport).
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	// URL connects to a server that already runs somewhere else. Mutually
	// exclusive with Command: a server is either spawned or reached, and
	// accepting both would leave which one wins to chance.
	URL string `json:"url,omitempty"`
	// Headers are sent on every request to a URL server, for bearer tokens
	// and the like. HeadersEnv reads a value from the environment instead, so
	// a credential need not sit in a config file.
	Headers    map[string]string `json:"headers,omitempty"`
	HeadersEnv map[string]string `json:"headers_env,omitempty"`
	// Enabled is false by default so adding a server to config is not the same
	// as authorizing it.
	Enabled bool `json:"enabled"`
	// AllowTools optionally restricts which of the server's tools are exposed.
	// Empty means all of them, which is the riskier choice.
	AllowTools []string `json:"allow_tools,omitempty"`
	// Digest is NOT YET ENFORCED. It is carried through configuration and
	// verified nowhere — connectOne does not read it — so setting it today
	// buys an air-gapped operator no protection at all. It is left in place
	// because the field is the right shape and removing it would break
	// configs that already set it, but anything that reads like a
	// supply-chain guarantee must not be claimed until this is wired up.
	//
	// Intended: pins the server artifact. Air-gapped installs must set this;
	// a tag or floating command is not reproducible (docs/ops/air-gap.md).
	Digest string `json:"digest,omitempty"`
}

// Gateway manages registered servers and exposes their tools to the agent.
type Gateway struct {
	mu      sync.RWMutex
	clients map[string]*Client
	configs map[string]ServerConfig
	// failed are the enabled servers that did not connect, with why.
	failed map[string]error
	// life is the context Connect was given: a server process lives as long
	// as it, whatever context a later Restart was called with.
	life context.Context
	// restarting serialises Restart, so two restarts of one server cannot
	// both start a process and leave one running unowned.
	restarting sync.Mutex

	// Confine, when set, builds each stdio server's command with its network
	// confined; MustConfine refuses a stdio server when it cannot be.
	Confine     Launcher
	MustConfine bool
	calls       serverCallers
}

func NewGateway() *Gateway {
	return &Gateway{clients: make(map[string]*Client), configs: make(map[string]ServerConfig), failed: map[string]error{}}
}

// Connect starts and initializes the enabled servers. A server that fails to
// start is reported but does not prevent the others from working: one broken
// integration should not take down the agent.
func (g *Gateway) Connect(ctx context.Context, configs []ServerConfig) []error {
	g.mu.Lock()
	g.life = ctx
	g.mu.Unlock()
	var errs []error
	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		if !validServerName.MatchString(cfg.Name) {
			// Not kept even as failed: the name would reach /mcp and the panels unescaped.
			errs = append(errs, fmt.Errorf("mcp server %+q: invalid server name (letters, digits, _ and - only, up to 64)", cfg.Name))
			continue
		}
		if err := g.connectOne(ctx, ctx, cfg); err != nil {
			g.mu.Lock()
			g.configs[cfg.Name], g.failed[cfg.Name] = cfg, err
			g.mu.Unlock()
			errs = append(errs, fmt.Errorf("mcp server %q: %w", cfg.Name, err))
		}
	}
	return errs
}

// connectOne starts a server whose process (or connection) lives as long as
// life, and initializes it within ctx.
func (g *Gateway) connectOne(life, ctx context.Context, cfg ServerConfig) error {
	if !validServerName.MatchString(cfg.Name) {
		return fmt.Errorf("invalid server name %q (letters, digits, _ and - only)", cfg.Name)
	}
	if cfg.Command == "" && cfg.URL == "" {
		return fmt.Errorf("no command or url configured")
	}
	if cfg.Command != "" && cfg.URL != "" {
		return fmt.Errorf("set either command or url, not both: " +
			"a server is spawned or reached, and which one wins should not be chance")
	}

	var transport Transport
	var err error
	if cfg.URL != "" {
		headers := map[string]string{}
		for k, v := range cfg.Headers {
			headers[k] = v
		}
		// Environment-sourced headers win, so a config file can name the
		// header without carrying the secret.
		for k, envVar := range cfg.HeadersEnv {
			if v := os.Getenv(envVar); v != "" {
				headers[k] = v
			} else {
				return fmt.Errorf("header %s: environment variable %s is not set", k, envVar)
			}
		}
		transport, err = NewHTTPTransport(life, HTTPConfig{URL: cfg.URL, Headers: headers})
	} else {
		transport, err = g.startStdio(life, cfg)
	}
	if err != nil {
		return err
	}
	client := NewClient(cfg.Name, transport)
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return err
	}

	g.mu.Lock()
	old := g.clients[cfg.Name]
	g.clients[cfg.Name] = client
	g.configs[cfg.Name] = cfg
	delete(g.failed, cfg.Name)
	g.mu.Unlock()
	if old != nil && old != client {
		_ = old.Close()
	}
	return nil
}

// Restart closes a configured server's connection, if any, and connects it
// again. Its tools keep their names and reach the new connection; tools the
// server added since are offered from the next session.
func (g *Gateway) Restart(ctx context.Context, name string) error {
	g.restarting.Lock()
	defer g.restarting.Unlock()
	g.mu.Lock()
	cfg, ok := g.configs[name]
	old := g.clients[name]
	delete(g.clients, name)
	life := g.life
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("no MCP server %q is configured and enabled", name)
	}
	if old != nil {
		_ = old.Close()
	}
	if life == nil {
		life = context.WithoutCancel(ctx)
	}
	if err := g.connectOne(life, ctx, cfg); err != nil {
		g.mu.Lock()
		g.failed[name] = err
		g.mu.Unlock()
		return err
	}
	return nil
}

// ServerStatus is one configured server as /mcp shows it.
type ServerStatus struct {
	Name      string
	Transport string // stdio or http
	Connected bool
	Err       error
	Tools     []string // the tools it offers that are allowed, by remote name
	// Refused are the tools it offers that were left out for their names, as
	// it sent them: untrusted text, for showing escaped.
	Refused []string
}

// Servers reports every enabled server, connected or not, by name.
func (g *Gateway) Servers() []ServerStatus {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []ServerStatus
	for name, cfg := range g.configs {
		st := ServerStatus{Name: name, Transport: "stdio", Err: g.failed[name]}
		if cfg.URL != "" {
			st.Transport = "http"
		}
		if c, ok := g.clients[name]; ok {
			st.Connected = true
			st.Refused = slices.Clone(c.Refused())
			for _, d := range c.Tools() {
				if allowed(cfg, d.Name) {
					st.Tools = append(st.Tools, d.Name)
				}
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// client is the live connection for a server, nil when it has none.
func (g *Gateway) client(name string) *Client {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.clients[name]
}

// ServerPrompt is one prompt a connected server offers.
type ServerPrompt struct {
	Server string
	Prompt PromptDef
}

// Prompts are the connected servers' prompts, by server then name.
func (g *Gateway) Prompts() []ServerPrompt {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []ServerPrompt
	for name, c := range g.clients {
		for _, p := range c.Prompts() {
			out = append(out, ServerPrompt{Server: name, Prompt: p})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Prompt.Name < out[j].Prompt.Name
	})
	return out
}

// GetPrompt fetches a prompt from a connected server; its text is untrusted.
func (g *Gateway) GetPrompt(ctx context.Context, server, name string, args map[string]string) (string, error) {
	c := g.client(server)
	if c == nil {
		return "", fmt.Errorf("MCP server %s is not connected; /mcp restart %s reconnects it", server, server)
	}
	return c.GetPrompt(ctx, name, args)
}

var validServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidServerName reports whether name may name a server: one that fails is
// never connected, so a view listing configured servers leaves it out too.
func ValidServerName(name string) bool { return validServerName.MatchString(name) }

// Tools returns the gateway's tools as agent-facing tools, namespaced by
// server so a malicious server cannot shadow a native tool.
func (g *Gateway) Tools() []tools.Tool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var out []tools.Tool
	names := make([]string, 0, len(g.clients))
	for name := range g.clients {
		names = append(names, name)
	}
	sort.Strings(names) // stable ordering keeps the prompt prefix cacheable

	for _, server := range names {
		client := g.clients[server]
		cfg := g.configs[server]
		for _, def := range client.Tools() {
			if !allowed(cfg, def.Name) {
				continue
			}
			out = append(out, &remoteTool{
				gw:          g,
				server:      server,
				remoteName:  def.Name,
				description: sanitizeDescription(def.Description),
				schema:      def.InputSchema,
			})
		}
	}
	return out
}

func allowed(cfg ServerConfig, tool string) bool {
	if len(cfg.AllowTools) == 0 {
		return true
	}
	for _, t := range cfg.AllowTools {
		if t == tool {
			return true
		}
	}
	return false
}

func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.clients {
		_ = c.Close()
	}
	g.clients = map[string]*Client{}
}

// Status reports connected servers and their tool counts, for `abhed doctor`.
func (g *Gateway) Status() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var out []string
	for name, c := range g.clients {
		out = append(out, fmt.Sprintf("%s (%d tools)", name, len(c.Tools())))
	}
	sort.Strings(out)
	return out
}

// remoteTool adapts an MCP tool to Abhed's tool interface.
type remoteTool struct {
	// gw is asked for the server's connection at each call, so a restart
	// reaches tools already handed out.
	gw          *Gateway
	server      string
	remoteName  string
	description string
	schema      json.RawMessage
}

// Name is namespaced: mcp__<server>__<tool>. Without this a server could
// register a tool called "bash" and intercept the agent's own calls.
func (t *remoteTool) Name() string {
	return fmt.Sprintf("mcp__%s__%s", t.server, t.remoteName)
}

func (t *remoteTool) Description() string { return t.description }

// ServerName and RemoteName are the two halves of Name, kept apart because a
// server name may itself hold "__".
func (t *remoteTool) ServerName() string { return t.server }
func (t *remoteTool) RemoteName() string { return t.remoteName }

func (t *remoteTool) Schema() json.RawMessage {
	if len(t.schema) == 0 {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return t.schema
}

// Mutates is conservatively true: Abhed cannot know what a third-party server
// does, so every MCP call routes through the policy engine for approval.
func (t *remoteTool) Mutates() bool { return true }

func (t *remoteTool) Run(ctx context.Context, _ *tools.Session, args json.RawMessage) tools.Result {
	client := t.gw.client(t.server)
	if client == nil {
		return tools.Result{Content: fmt.Sprintf("MCP server %s is not connected; /mcp restart %s reconnects it", t.server, t.server), IsError: true}
	}
	defer t.gw.calls.begin(t.server, egress.CallerOf(ctx))()
	content, isErr, err := client.Call(ctx, t.remoteName, args)
	if err != nil {
		return tools.Result{
			Content: fmt.Sprintf("MCP call to %s failed: %v", t.Name(), err),
			IsError: true,
		}
	}
	if content == "" {
		content = "[no content returned]"
	}
	// The response is untrusted third-party data. The loop tags the observation
	// accordingly; here we bound its size so one server cannot flood context.
	const maxContent = 30000
	truncated := false
	if len(content) > maxContent {
		content = content[:maxContent] + "\n\n[truncated]"
		truncated = true
	}
	return tools.Result{Content: content, IsError: isErr, Truncated: truncated}
}

// sanitizeDescription defends against tool poisoning.
//
// A tool description is written by a third party and injected verbatim into the
// model's context, which makes it an instruction-injection surface. This strips
// the patterns that try to exploit that and bounds the length so one server
// cannot dominate the prompt.
func sanitizeDescription(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no description provided)"
	}

	// Neutralize instruction-injection phrasing aimed at the model rather than
	// describing the tool.
	for _, pattern := range injectionPatterns {
		s = pattern.ReplaceAllString(s, "[redacted]")
	}

	// Collapse control characters that could forge message boundaries.
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")

	const maxDesc = 1024
	if len(s) > maxDesc {
		s = s[:maxDesc] + "…"
	}
	return s
}

var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all |any |the )?(previous|prior|above|preceding) instructions?`),
	regexp.MustCompile(`(?i)disregard (all |any |the )?(previous|prior|above)`),
	regexp.MustCompile(`(?i)you are now (a|an|in) `),
	regexp.MustCompile(`(?i)</?(system|assistant|user)>`),
	regexp.MustCompile(`(?i)\bnew instructions?\b`),
	regexp.MustCompile(`(?i)do not (tell|inform|mention to) the user`),
	regexp.MustCompile(`(?i)without (asking|informing|telling) the user`),
}
