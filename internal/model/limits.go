package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Where a profile's context window came from, from most to least trusted.
const (
	// WindowConfig is the provider's context_window in the configuration.
	WindowConfig = "config"
	// WindowLearned is the limit a provider named when it refused a request
	// as too long, or the one a truncated prompt showed. It only ever lowers
	// the window, the configured one included: the server is the authority.
	WindowLearned = "learned"
	// WindowDiscovered is what the serving endpoint reported about itself.
	WindowDiscovered = "discovered"
	// WindowBuiltIn is Abhed's own table of published limits.
	WindowBuiltIn = "built-in"
	// WindowFallback is FallbackWindow, for a model nothing above knows.
	WindowFallback = "fallback"
)

// FallbackWindow is the window assumed for a model Abhed knows nothing about.
//
// 32,768 tokens is the smallest window among the instruction-tuned models
// that can carry an agent's system prompt and tool schemas with room to work.
// Erring small costs earlier compaction and a tighter cap on each tool result;
// erring large costs a refused request, and with an unknown limit the session
// could not tell how far over it was. A provider that names a smaller limit
// when it refuses a request lowers the window for the rest of the session.
const FallbackWindow = 32768

// MinWindow is the smallest window learned or discovered that is believed.
// An endpoint that names less (an error reading "> 5 maximum") would leave
// every tool result cut to a few characters and a compaction on every turn;
// no model that can run an agent has a window this small.
const MinWindow = 2048

// DefaultOutputReserve is the room kept for a reply when neither the
// configuration nor the request names an output cap: the Anthropic adapter's
// own default max_tokens, and a typical reply with a tool call or two.
const DefaultOutputReserve = 8192

// Limits is an adapter's context window and output cap as they stand, shared
// by every copy of its profile. It starts from the configuration, the
// built-in table or the fallback, and moves when the endpoint says otherwise:
// discovery raises or lowers a window that was not configured, and a refusal
// or a truncated prompt lowers it.
type Limits struct {
	mu     sync.RWMutex
	window int
	output int
	source string
	// typ and base say how to ask the endpoint about itself.
	typ, base string
	// local is an endpoint on this machine, whose server decides the window
	// at run time whatever the model was trained for.
	local bool
	// discovered counts the attempts made to ask the endpoint; see Discover.
	attempts int
	// silentTruncation marks a server that cuts an over-long prompt rather
	// than refusing it (Ollama).
	silentTruncation bool
	// key names the endpoint, model and account in the window memory.
	key string
	// learnedAt is when a learned window was named, by this process or,
	// remembered, by an earlier one.
	learnedAt time.Time
}

// WindowMemory keeps the limits providers named across processes, so a
// restarted session does not pay one refused call to learn the same limit
// again. The program installs the person's own (SetWindowMemory); without
// one, learned limits last as long as the process.
type WindowMemory interface {
	// Recall is the window remembered for key and when it was named, or 0.
	Recall(key string) (window int, at time.Time)
	// Remember keeps window for key.
	Remember(key string, window int)
	// Forget drops what is kept for key.
	Forget(key string)
}

// MinRememberedWindow is the smallest limit kept beyond the process. A
// smaller one is believed for the session (down to MinWindow) but not made
// permanent: a gateway that once relays a fallback model's small limit
// would otherwise pin every later session to it. No hosted model that runs
// an agent has a window under 16,384.
const MinRememberedWindow = 16_384

var windowMemory atomic.Pointer[WindowMemory]

// SetWindowMemory installs the memory learned limits are kept in; nil
// removes it.
func SetWindowMemory(m WindowMemory) {
	if m == nil {
		windowMemory.Store(nil)
		return
	}
	windowMemory.Store(&m)
}

func memory() WindowMemory {
	if m := windowMemory.Load(); m != nil {
		return *m
	}
	return nil
}

// WindowKey names an endpoint's model for one account in the window memory,
// as a hash: the provider type, the endpoint without its user, password,
// query or fragment (where a credential may be written), the model id, and
// the credential itself, so two accounts on one endpoint keep their own
// limits. Nothing of them can be read back from the key; a credential is a
// long random string, so the hash cannot be searched for it either.
func WindowKey(typ, base, model, credential string) string {
	h := sha256.New()
	for _, part := range []string{"abhed-window-v1", strings.ToLower(typ), endpointOnly(base), model, credential} {
		fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// endpointOnly is a base URL with only its scheme, host and path, lower-cased
// where case does not matter.
func endpointOnly(base string) string {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/")
}

// credentialOf is the credential an adapter sends, which names the account.
func credentialOf(a Adapter) string {
	switch c := a.(type) {
	case *Anthropic:
		return orElse(c.Bearer, c.APIKey)
	case *OpenAICompatible:
		return c.APIKey
	case *Gemini:
		return c.APIKey
	case *WatsonX:
		return c.APIKey
	}
	return ""
}

// resolveLimits is the starting window for a spec: the configuration's,
// else the table's, else the fallback.
func resolveLimits(s Spec, base, credential string) *Limits {
	l := &Limits{typ: s.Type, base: base, local: isLocalEndpoint(s.Type, base), key: WindowKey(s.Type, base, s.Model, credential)}
	l.silentTruncation = isOllama(s.Type, base)
	_, out, known := LookupLimits(s.Model)
	l.output = out
	switch {
	case s.ContextWindow > 0:
		l.window, l.source = s.ContextWindow, WindowConfig
	case known:
		w, _, _ := LookupLimits(s.Model)
		l.window, l.source = w, WindowBuiltIn
		if l.local {
			// A local server runs a model with the window it was started
			// with (Ollama's default is 4,096), not the one it was trained
			// for: the trained window is a ceiling until the server says.
			l.window, l.source = min(w, FallbackWindow), WindowFallback
		}
	default:
		l.window, l.source = FallbackWindow, WindowFallback
	}
	// A limit a provider named for this endpoint, model and account in an
	// earlier process. It does not override a configured window: the
	// operator's statement outranks a relayed error from another day, while
	// a refusal in this process still lowers it.
	if m := memory(); m != nil && l.source != WindowConfig {
		if w, at := m.Recall(l.key); w >= MinRememberedWindow && w < l.window {
			l.window, l.source, l.learnedAt = w, WindowLearned, at
		}
	}
	if s.MaxOutputTokens > 0 && (l.output == 0 || s.MaxOutputTokens < l.output) {
		// A configured cap is the cap; one above the model's is clamped.
		l.output = s.MaxOutputTokens
	}
	return l
}

// isLocalEndpoint reports a model server on this machine: a local server
// type, or the generic OpenAI-compatible type at a loopback address. A
// hosted API's type reached through a local proxy keeps its model's window.
func isLocalEndpoint(typ, base string) bool {
	if LocalByDefault(typ) {
		return true
	}
	if typ != "openai-compatible" {
		return false
	}
	b := strings.ToLower(base)
	for _, h := range []string{"://localhost", "://127.", "://[::1]", "://0.0.0.0"} {
		if strings.Contains(b, h) {
			return true
		}
	}
	return false
}

// isOllama reports an Ollama server: the ollama type, or any endpoint on
// Ollama's own port.
func isOllama(typ, base string) bool {
	return typ == "ollama" || strings.Contains(base, ":11434")
}

// snapshot is the limits as they stand.
func (l *Limits) snapshot() (window, output int, source string) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.window, l.output, l.source
}

// apply is p with the limits in force written in.
func (l *Limits) apply(p Profile) Profile {
	if l == nil {
		return p
	}
	w, out, src := l.snapshot()
	p.ContextWindow, p.WindowSource, p.OutputLimit = w, src, out
	l.mu.RLock()
	p.LearnedAt = l.learnedAt
	l.mu.RUnlock()
	p.SilentTruncation = l.silentTruncation
	if out > 0 && p.MaxOutputTokens > out {
		p.MaxOutputTokens = out
	}
	return p
}

// learn lowers the window to limit, as a provider's refusal named it. It
// reports whether the window changed.
func (l *Limits) learn(limit int) bool {
	if l == nil || limit < MinWindow {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window > 0 && limit >= l.window {
		return false
	}
	l.window, l.source, l.learnedAt = limit, WindowLearned, time.Now()
	return true
}

// discovered takes what the endpoint reported. A configured or learned
// window stands; anything else gives way to the endpoint's own figure.
func (l *Limits) discovered(window, output int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	if window < MinWindow {
		window = 0
	}
	if window > 0 && l.source != WindowConfig && l.source != WindowLearned && window != l.window {
		l.window, l.source, changed = window, WindowDiscovered, true
	}
	if window > 0 && l.source == WindowBuiltIn {
		l.source = WindowDiscovered
	}
	if output > 0 && (l.output == 0 || output < l.output) {
		l.output = output
	}
	return changed
}

// limited is an adapter that carries Limits.
type limited interface {
	limitsRef() **Limits
}

func (c *Anthropic) limitsRef() **Limits        { return &c.limits }
func (c *OpenAICompatible) limitsRef() **Limits { return &c.limits }
func (g *Gemini) limitsRef() **Limits           { return &g.limits }
func (w *WatsonX) limitsRef() **Limits          { return &w.limits }

// limitsOf is the Limits an adapter carries, or nil.
func limitsOf(a Adapter) *Limits {
	if h, ok := a.(limited); ok {
		return *h.limitsRef()
	}
	return nil
}

// attachLimits gives an adapter built from s the limits s resolves to.
func attachLimits(a Adapter, s Spec) {
	h, ok := a.(limited)
	if !ok {
		return
	}
	*h.limitsRef() = resolveLimits(s, Endpoint(a), credentialOf(a))
}

// LearnWindow lowers an adapter's window to limit, for every session sharing
// the adapter, and reports whether it changed. named marks a limit the
// provider stated, which is kept in the window memory for later processes;
// one Abhed inferred (from a cut prompt, or below a refusal that named none)
// lasts only as long as the process.
func LearnWindow(a Adapter, limit int, named bool) bool {
	l := limitsOf(a)
	if !l.learn(limit) {
		return false
	}
	if m := memory(); named && m != nil && limit >= MinRememberedWindow {
		m.Remember(l.key, limit)
	}
	return true
}

// ResolveLimits is the window, output cap and source a provider spec starts
// with, before anything is discovered, for listing models without building
// or reaching them.
func ResolveLimits(s Spec) (window, output int, source string) {
	base := s.BaseURL
	if base == "" {
		base = LocalDefaultURL(s.Type)
	}
	return resolveLimits(s, base, s.APIKey).snapshot()
}

// OutputReserve is the room a reply needs in the window: the request's cap,
// else the profile's, else DefaultOutputReserve, never above the model's
// own maximum.
func OutputReserve(p Profile, requested int) int {
	n := requested
	if n <= 0 {
		n = p.MaxOutputTokens
	}
	if n <= 0 {
		n = DefaultOutputReserve
	}
	if p.OutputLimit > 0 && n > p.OutputLimit {
		n = p.OutputLimit
	}
	return n
}

// clampOutput keeps a requested output cap within the model's maximum, so a
// cap set above it is not a refused request.
func clampOutput(n int, p Profile) int {
	if n > 0 && p.OutputLimit > 0 && n > p.OutputLimit {
		return p.OutputLimit
	}
	return n
}

// WindowNote is what to tell a person about a window Abhed had to assume,
// or "" for one it knows.
func WindowNote(p Profile) string {
	if p.WindowSource != WindowFallback {
		return ""
	}
	note := "the context window of " + p.Name + " is not known, so Abhed assumes " +
		commaInt(p.ContextWindow) + " tokens and compacts early; set context_window on the provider to use the model's full window"
	if p.SilentTruncation {
		note += " (an Ollama server's window is its OLLAMA_CONTEXT_LENGTH or the model's num_ctx, read once the model is loaded)"
	}
	return note
}

func commaInt(n int) string {
	d := strconv.Itoa(n)
	var b strings.Builder
	for i := range d {
		if i > 0 && (len(d)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(d[i])
	}
	return b.String()
}

// ForgetWindow drops the limit kept for an adapter's endpoint, model and
// account, so the next session starts from the configuration, the endpoint
// or the table again. It reports whether a memory is installed.
func ForgetWindow(a Adapter) bool {
	l, m := limitsOf(a), memory()
	if l == nil || m == nil {
		return false
	}
	m.Forget(l.key)
	return true
}
