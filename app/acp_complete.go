package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/secretfiles"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// An editor's inline completion (studio-acp-contract.md §7.10): the code
// around the cursor goes to the configured completion model, which answers
// with text to insert. No tools, no side effects; not recorded one by one.

func init() {
	liveFeatures = append(liveFeatures, "complete")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/complete":        (*acpConn).complete,
		"_abhed/complete/cancel": (*acpConn).completeCancel,
	})
}

// Completion bounds: what is sent, how often, and how long it may take.
const (
	completePrefix    = 6000 // characters before the cursor
	completeSuffix    = 2000 // characters after it
	completeFiles     = 3
	completeFileLines = 60
	completeFileBytes = 4 << 10
	completeAllFiles  = 12 << 10
	completeMaxTokens = 128
	completeTimeout   = 10 * time.Second
)

// completeHold waits for a newer request to replace this one (ctx ends) or
// for the debounce to pass; tests replace it.
var completeHold = func(ctx context.Context) error {
	t := time.NewTimer(60 * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Rate limits: per session, and per connection over all its sessions.
var (
	sessionRate    = rateLimit{burst: 5, every: 200 * time.Millisecond}
	connectionRate = rateLimit{burst: 10, every: 100 * time.Millisecond}
)

// completeNow is the clock the rate limits read; tests replace it.
var completeNow = time.Now

// fimTypes are the provider types served with a fill-in-the-middle endpoint;
// any other is asked through a chat turn.
var fimTypes = map[string]bool{"ollama": true, "vllm": true, "llamacpp": true, "tgi": true, "sglang": true, "openai-compatible": true}

// rateLimit is a token bucket's shape.
type rateLimit struct {
	burst float64
	every time.Duration
}

// bucket is a token bucket of a rateLimit.
type bucket struct {
	tokens   float64
	refilled time.Time
}

// take takes one token at now, refilling first, and reports whether one was there.
func (b *bucket) take(l rateLimit, now time.Time) bool {
	if b.refilled.IsZero() {
		b.tokens, b.refilled = l.burst, now
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.refilled))/float64(l.every))
	b.refilled = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// acpCompleter is one session's completions: its model, its rate limit, the
// request in flight, and what the summary counts.
type acpCompleter struct {
	mu       sync.Mutex
	provider string
	adapter  model.Adapter
	chatOnly bool // the provider's completions endpoint refused fill in the middle
	rate     bucket
	cancel   context.CancelFunc
	latest   int64  // the newest request's sequence
	mode     string // the mode completion.enabled last recorded
	closed   bool   // the session has closed; nothing more is sent
	sum      agent.CompletionSummary
}

type completeRequest struct {
	URI      string `json:"uri"`
	Position struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"position"`
	Prefix   string `json:"prefix"`
	Suffix   string `json:"suffix"`
	Language string `json:"language"`
	Trigger  string `json:"trigger"`
	// Seq orders a session's requests as Studio sent them; without it they
	// are ordered as they arrive.
	Seq     int64 `json:"seq"`
	Context []struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"context"`
}

// completeOff says why completions are off for cfg, "" when they are on,
// with the provider resolved as a session's model is: its api_key_env read,
// its settings checked, and a managed model pin honoured.
func completeOff(cfg config.Config) (string, config.ProviderConfig) {
	name := cfg.Model.Completion
	if name == "" {
		return "no completion model is configured (model.completion)", config.ProviderConfig{}
	}
	if !cfg.CompletionsOn() {
		return "completions are turned off (completion.enabled)", config.ProviderConfig{}
	}
	if !cfg.Offered(name) {
		return "model.completion names " + name + ", which is not a configured provider", config.ProviderConfig{}
	}
	// A managed model pin binds completions too, unless the managed file names their model itself.
	if cfg.ModelPinned() && name != cfg.Model.Default && !cfg.ManagedSets("model.completion") {
		return "the managed configuration sets the model to " + cfg.Model.Default + ", and does not name a completion model", config.ProviderConfig{}
	}
	p, err := cfg.ProviderNamed(name)
	if err != nil {
		return "the completion model cannot be used: " + err.Error(), config.ProviderConfig{}
	}
	if cfg.Completion.LocalOnly && !p.OnThisMachine() {
		return "completions are kept to a model on this machine (completion.local_only), and " + name + " is not one", config.ProviderConfig{}
	}
	return "", p
}

// completionAdapter builds the provider's adapter for completions: no key
// but the one it names, each request sent once, and under local_only no
// redirect off this machine. An adapter that cannot take that is refused.
func completionAdapter(p config.ProviderConfig, localOnly bool) (model.Adapter, error) {
	spec := p.Spec()
	spec.NoAmbientKey = true
	a, err := model.New(spec)
	if err != nil {
		return nil, err
	}
	var redirect func(*http.Request, []*http.Request) error
	if localOnly {
		redirect = func(req *http.Request, _ []*http.Request) error {
			if !(config.ProviderConfig{BaseURL: req.URL.String()}).OnThisMachine() {
				return fmt.Errorf("a redirect to %s leaves this machine (completion.local_only)", req.URL.Host)
			}
			return nil
		}
	}
	if !model.SendOnce(a, redirect) {
		return nil, fmt.Errorf("provider type %s cannot be held to one request on this machine", p.Type)
	}
	return a, nil
}

// noFIM matches a 400's body that says the endpoint or a parameter is not
// supported, as opposed to a bad request of another kind.
var noFIM = regexp.MustCompile(`(?i)not (?:currently )?supported|unsupported|does not support|unrecogni[sz]ed|unknown (?:field|param|argument)|extra (?:fields|inputs) not permitted`)

// fimRefused reports an answer that says the endpoint does not fill in the middle.
func fimRefused(err error) bool {
	var se *model.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest:
		return noFIM.MatchString(se.Body)
	}
	return false
}

// sendable is the file a URI names when it may be sent, else why not: under
// one of roots in every form it has (as named, and with links followed),
// and in none of them a file of keys or Abhed's own state.
func sendable(uri string, roots []string) (string, string) {
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") || (u.Host != "" && u.Host != "localhost") {
		return "", "the document is not a file"
	}
	path := filepath.Clean(u.Path)
	forms := sandbox.PathForms(path)
	if len(forms) == 0 {
		return "", "the document is not a file"
	}
	var rootForms []string
	for _, r := range roots {
		rootForms = append(rootForms, sandbox.PathForms(r)...)
	}
	for _, f := range forms {
		inside := false
		for _, r := range rootForms {
			if _, ok := sandbox.Within(f, r); ok {
				inside = true
				break
			}
		}
		switch {
		case !inside:
			return "", "the file is not in the session's workspace"
		case secretfiles.Match(f) != "":
			return "", "the file looks like it holds keys or credentials"
		case tools.IsState(f, roots...):
			return "", "the file is Abhed's own state"
		}
	}
	return path, ""
}

// withheldAround cleans the text on both sides of the cursor as one, so a
// secret the cursor splits is withheld whole, and splits it back. Where the
// cleaned whole starts with the cleaned prefix, the cursor is exactly there;
// otherwise a redaction spans it, and the suffix is what the cleaned whole
// ends with in common with the cleaned suffix. Each side is cleaned again,
// so what one side alone shows as a secret is withheld too.
func withheldAround(clean func(string) string, prefix, suffix string) (string, string) {
	whole, before, after := clean(prefix+suffix), clean(prefix), clean(suffix)
	cut := len(before)
	if !strings.HasPrefix(whole, before) {
		n := 0
		for n < len(after) && n < len(whole) && whole[len(whole)-1-n] == after[len(after)-1-n] {
			n++
		}
		cut = len(whole) - n
		for cut < len(whole) && !utf8.RuneStart(whole[cut]) {
			cut++
		}
	}
	return clean(whole[:cut]), clean(whole[cut:])
}

// headRunes is s cut to at most n characters from its start; tailRunes from its end.
func headRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for k := 0; k < n; k++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

func tailRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := len(s)
	for k := 0; k < n; k++ {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}

// headBytes is s cut to at most n bytes on a character boundary.
func headBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// completer is the session's, or nil once the session has closed.
func (c *acpConn) completer(s *acpSession) *acpCompleter {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	c.compMu.Lock()
	defer c.compMu.Unlock()
	if closed {
		return nil
	}
	if c.comps == nil {
		c.comps = map[string]*acpCompleter{}
	}
	k := c.comps[s.id]
	if k == nil {
		k = &acpCompleter{}
		c.comps[s.id] = k
	}
	return k
}

func (c *acpConn) complete(msg rpcMessage) {
	var req completeRequest
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		c.reply(msg.ID, nil, badParams("", err))
		return
	}
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	empty := func(field, why string) {
		c.reply(msg.ID, map[string]any{"items": []any{}, field: why}, nil)
	}
	cfg := parts.Config
	off, provider := completeOff(cfg)
	if off != "" {
		empty("disabled", off)
		return
	}
	k := c.completer(s)
	if k == nil {
		c.reply(msg.ID, nil, refusal(errParams, "the session has closed"))
		return
	}
	refuse := func(why string) {
		k.mu.Lock()
		k.sum.Refused++
		k.mu.Unlock()
		empty("refused", why)
	}
	roots := append([]string{parts.Session.Root}, s.roots()...)
	path, why := sendable(req.URI, roots)
	if why == "" && readDenied(parts.Loop.Policy, path) {
		why = "a rule keeps this file from being read"
	}
	if why != "" {
		refuse(why)
		return
	}

	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		c.reply(msg.ID, nil, refusal(errParams, "the session has closed"))
		return
	}
	now := completeNow()
	// The session's limit first, so a session over it spends none of the connection's.
	if !k.rate.take(sessionRate, now) || !c.compRateTake(now, &k.rate) {
		k.sum.Refused++
		k.mu.Unlock()
		empty("limited", "too many completions; slow down")
		return
	}
	// A newer request replaces the one in flight; an older one arriving late is dropped.
	seq := req.Seq
	if seq == 0 {
		seq = k.latest + 1
	}
	if seq <= k.latest {
		k.sum.Superseded++
		k.mu.Unlock()
		empty("cancelled", "a newer completion replaced it")
		return
	}
	k.latest = seq
	if k.cancel != nil {
		k.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	k.cancel = cancel
	if k.adapter == nil || k.provider != cfg.Model.Completion {
		a, err := completionAdapter(provider, cfg.Completion.LocalOnly)
		if err != nil {
			k.mu.Unlock()
			cancel()
			c.reply(msg.ID, nil, refusal(errRefused, "the completion model cannot be used: %v", err))
			return
		}
		k.adapter, k.provider, k.chatOnly = a, cfg.Model.Completion, false
		k.sum.Provider, k.sum.Model = k.provider, provider.Model
	}
	a := k.adapter
	_, canFIM := a.(model.Infiller)
	fim := canFIM && fimTypes[provider.Type] && !k.chatOnly
	k.mu.Unlock()
	defer cancel()

	if completeHold(ctx) != nil {
		k.count(func(s *agent.CompletionSummary) { s.Superseded++ })
		empty("cancelled", "a newer completion replaced it")
		return
	}

	// Stored secrets, and anything shaped like a key, never leave: withheld
	// from the whole text before it is cut, so a cut cannot split one, and
	// across the cursor, so neither side keeps part of one.
	clean := func(text string) string {
		text = parts.Loop.Withheld(text)
		text, _ = secretfiles.RedactContent(text)
		return text
	}
	// Only what is near the cursor goes, and other open files only as far as given.
	before, after := withheldAround(clean, req.Prefix, req.Suffix)
	prefix := tailRunes(before, completePrefix)
	suffix := headRunes(after, completeSuffix)
	var lead strings.Builder
	used := 0
	for _, f := range req.Context {
		if used == completeFiles || lead.Len() >= completeAllFiles {
			break
		}
		fp, why := sendable(f.URI, roots)
		if why != "" || fp == path || readDenied(parts.Loop.Policy, fp) {
			continue
		}
		lines := strings.SplitN(clean(f.Text), "\n", completeFileLines+1)
		if len(lines) > completeFileLines {
			lines = lines[:completeFileLines]
		}
		rel, _ := filepath.Rel(parts.Session.Root, fp)
		part := "// " + filepath.ToSlash(rel) + "\n" + headBytes(strings.Join(lines, "\n"), completeFileBytes) + "\n\n"
		lead.WriteString(headBytes(part, completeAllFiles-lead.Len()))
		used++
	}
	sentPrefix, sentSuffix := lead.String()+prefix, suffix
	if ctx.Err() != nil {
		k.count(func(s *agent.CompletionSummary) { s.Superseded++ })
		empty("cancelled", "a newer completion replaced it")
		return
	}
	// The record names where the code goes, and how, before any of it goes;
	// a record that cannot be written, or a session that has closed, stops it.
	mode := "chat"
	if fim {
		mode = "fim"
	}
	if why := c.completeEnabled(s, k, provider, mode); why != "" {
		refuse(why)
		return
	}
	in := model.InfillRequest{Prefix: sentPrefix, Suffix: sentSuffix, Language: req.Language, MaxTokens: completeMaxTokens, Stop: []string{"\n\n\n"}}
	var res model.InfillResult
	var err error
	if fim {
		res, err = a.(model.Infiller).Infill(ctx, in)
		if fimRefused(err) && ctx.Err() == nil {
			// No fill in the middle here: chat from now on, recorded as such first.
			k.mu.Lock()
			k.chatOnly = true
			k.mu.Unlock()
			if why := c.completeEnabled(s, k, provider, "chat"); why != "" {
				k.count(func(s *agent.CompletionSummary) { s.Requests++; s.Failed++ })
				empty("refused", why)
				return
			}
			res, err = model.InfillByChat(ctx, a, in)
		}
	} else {
		res, err = model.InfillByChat(ctx, a, in)
	}
	// A reply that carries a secret is dropped, not edited into the code.
	text := res.Text
	if clean(text) != text {
		text = ""
	}
	cancelled := err != nil && ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded)
	k.mu.Lock()
	k.sum.Requests++
	k.sum.TokensIn += res.InputTokens
	k.sum.TokensOut += res.OutputTokens
	switch {
	case cancelled:
		k.sum.Cancelled++
	case err != nil:
		k.sum.Failed++
	case strings.TrimSpace(text) != "":
		k.sum.Served++
	}
	k.mu.Unlock()
	switch {
	case cancelled:
		empty("cancelled", "a newer completion replaced it")
	case err != nil:
		c.reply(msg.ID, nil, refusal(errRefused, "the completion model did not answer: %s", redacted(err.Error())))
	case strings.TrimSpace(text) == "" && res.Dropped != "":
		// Said, so the editor can tell a model that answers in prose from one with nothing to add.
		c.reply(msg.ID, map[string]any{"items": []any{}, "dropped": res.Dropped}, nil)
	case strings.TrimSpace(text) == "":
		c.reply(msg.ID, map[string]any{"items": []any{}}, nil)
	default:
		c.reply(msg.ID, map[string]any{"items": []map[string]any{{"text": text}}}, nil)
	}
}

// completeEnabled records completion.enabled before the first request of a
// session goes, and again if the mode changes; it says why not to send
// ("" to send): the record did not take it, or the session has closed.
func (c *acpConn) completeEnabled(s *acpSession, k *acpCompleter, p config.ProviderConfig, mode string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return "the session has closed"
	}
	if k.mode == mode {
		return ""
	}
	if _, err := s.parts.Loop.Recorder.Record(agent.EvCompletionEnabled, agent.ActorUser, agent.Trusted,
		agent.CompletionEnabled{Provider: k.provider, Model: p.Model, Local: p.OnThisMachine(), Mode: mode}); err != nil {
		return "the record did not take the completion, so nothing was sent"
	}
	k.mode = mode
	return ""
}

// compRateTake takes one from the connection's rate limit, or gives the
// session's token back when there is none.
func (c *acpConn) compRateTake(now time.Time, session *bucket) bool {
	c.compMu.Lock()
	defer c.compMu.Unlock()
	if c.compRate.take(connectionRate, now) {
		return true
	}
	session.tokens++
	return false
}

func (k *acpCompleter) count(f func(*agent.CompletionSummary)) {
	k.mu.Lock()
	f(&k.sum)
	k.mu.Unlock()
}

func (c *acpConn) completeCancel(msg rpcMessage) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if k := c.completer(s); k != nil {
		k.mu.Lock()
		if k.cancel != nil {
			k.cancel()
		}
		k.mu.Unlock()
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

// completionSummary records the session's completions, once, as it closes.
func (c *acpConn) completionSummary(s *acpSession) {
	c.compMu.Lock()
	k := c.comps[s.id]
	delete(c.comps, s.id)
	c.compMu.Unlock()
	if k == nil {
		return
	}
	k.mu.Lock()
	k.closed = true
	if k.cancel != nil {
		k.cancel()
	}
	sum := k.sum
	k.mu.Unlock()
	if sum.Requests+sum.Refused+sum.Cancelled+sum.Superseded == 0 {
		return
	}
	if s.inner {
		if _, err := s.parts.Loop.Recorder.Record(agent.EvCompletionSummary, agent.ActorSystem, agent.Trusted, sum); err != nil {
			warnf("the record did not take %s: %v", agent.EvCompletionSummary, err)
		}
	}
}
