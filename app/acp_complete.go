package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/secretfiles"
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
	completePrefix     = 6000
	completeSuffix     = 2000
	completeFiles      = 3
	completeFileLines  = 60
	completeMaxTokens  = 128
	completeTimeout    = 10 * time.Second
	completeBurst      = 5
	completeRefillEach = 200 * time.Millisecond
)

// completeDebounce is how long a request waits for a newer one to replace it.
var completeDebounce = 60 * time.Millisecond

// fimTypes are the provider types served with a fill-in-the-middle endpoint;
// any other is asked through a chat turn.
var fimTypes = map[string]bool{"ollama": true, "vllm": true, "llamacpp": true, "tgi": true, "sglang": true, "openai-compatible": true}

// acpCompleter is one session's completions: its model, its rate limit, the
// request in flight, and what the summary counts.
type acpCompleter struct {
	mu       sync.Mutex
	provider string
	adapter  model.Adapter
	tokens   float64
	refilled time.Time
	cancel   context.CancelFunc
	gen      int
	enabled  bool // completion.enabled is recorded
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
	Context  []struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"context"`
}

// completeOff says why completions are off for cfg, "" when they are on.
func completeOff(cfg config.Config) (string, config.ProviderConfig) {
	if cfg.Model.Completion == "" {
		return "no completion model is configured (model.completion)", config.ProviderConfig{}
	}
	if !cfg.CompletionsOn() {
		return "completions are turned off (completion.enabled)", config.ProviderConfig{}
	}
	p, ok := cfg.Model.Providers[cfg.Model.Completion]
	if !ok {
		return "model.completion names no configured provider", config.ProviderConfig{}
	}
	if cfg.Completion.LocalOnly && !p.OnThisMachine() {
		return "completions are kept to a model on this machine (completion.local_only), and " + cfg.Model.Completion + " is not one", config.ProviderConfig{}
	}
	return "", p
}

// filePath is a file URI's path when it is under one of roots.
func filePath(uri string, roots []string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") || (u.Host != "" && u.Host != "localhost") {
		return "", false
	}
	p := filepath.Clean(u.Path)
	for _, r := range roots {
		if rel, err := filepath.Rel(filepath.Clean(r), p); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return p, true
		}
	}
	return "", false
}

func (c *acpConn) completer(sessionID string) *acpCompleter {
	c.compMu.Lock()
	defer c.compMu.Unlock()
	if c.comps == nil {
		c.comps = map[string]*acpCompleter{}
	}
	k := c.comps[sessionID]
	if k == nil {
		k = &acpCompleter{tokens: completeBurst, refilled: time.Now()}
		c.comps[sessionID] = k
	}
	return k
}

// allow takes one from the session's rate limit.
func (k *acpCompleter) allow(now time.Time) bool {
	k.tokens = min(completeBurst, k.tokens+float64(now.Sub(k.refilled))/float64(completeRefillEach))
	k.refilled = now
	if k.tokens < 1 {
		return false
	}
	k.tokens--
	return true
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
	off, provider := completeOff(parts.Config)
	if off != "" {
		empty("disabled", off)
		return
	}
	k := c.completer(s.id)
	roots := append([]string{parts.Session.Root}, s.roots()...)
	path, ok := filePath(req.URI, roots)
	refuse := func(why string) {
		k.mu.Lock()
		k.sum.Refused++
		k.mu.Unlock()
		empty("refused", why)
	}
	switch {
	case !ok:
		refuse("the document is not a file in the session's workspace")
		return
	case readDenied(parts.Loop.Policy, path):
		refuse("a rule keeps this file from being read")
		return
	case secretfiles.Match(path) != "":
		refuse("this file looks like it holds keys or credentials")
		return
	}

	// Only what is near the cursor goes, and other open files only as far as given.
	prefix := req.Prefix
	if len(prefix) > completePrefix {
		prefix = prefix[len(prefix)-completePrefix:]
	}
	suffix := req.Suffix
	if len(suffix) > completeSuffix {
		suffix = suffix[:completeSuffix]
	}
	var lead strings.Builder
	used := 0
	for _, f := range req.Context {
		if used == completeFiles {
			break
		}
		fp, ok := filePath(f.URI, roots)
		if !ok || fp == path || readDenied(parts.Loop.Policy, fp) || secretfiles.Match(fp) != "" {
			continue
		}
		lines := strings.SplitN(f.Text, "\n", completeFileLines+1)
		if len(lines) > completeFileLines {
			lines = lines[:completeFileLines]
		}
		rel, _ := filepath.Rel(parts.Session.Root, fp)
		lead.WriteString("// " + filepath.ToSlash(rel) + "\n" + strings.Join(lines, "\n") + "\n\n")
		used++
	}

	k.mu.Lock()
	if !k.allow(time.Now()) {
		k.sum.Refused++
		k.mu.Unlock()
		empty("limited", "too many completions; slow down")
		return
	}
	// A newer request replaces the one in flight.
	if k.cancel != nil {
		k.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	k.gen++
	gen := k.gen
	k.cancel = cancel
	if k.adapter == nil || k.provider != parts.Config.Model.Completion {
		a, err := provider.Adapter()
		if err != nil {
			k.mu.Unlock()
			cancel()
			c.reply(msg.ID, nil, refusal(errRefused, "the completion model cannot be used: %v", err))
			return
		}
		k.adapter, k.provider = a, parts.Config.Model.Completion
		k.sum.Provider, k.sum.Model = k.provider, provider.Model
	}
	a := k.adapter
	_, fim := a.(model.Infiller)
	fim = fim && fimTypes[provider.Type]
	first := !k.enabled
	k.enabled = true
	k.mu.Unlock()
	defer cancel()

	select {
	case <-time.After(completeDebounce):
	case <-ctx.Done():
		k.count(func(s *agent.CompletionSummary) { s.Cancelled++ })
		empty("cancelled", "a newer completion replaced it")
		return
	}
	if first {
		mode := "chat"
		if fim {
			mode = "fim"
		}
		s.record(agent.EvCompletionEnabled, agent.CompletionEnabled{Provider: k.provider, Model: provider.Model, Local: provider.OnThisMachine(), Mode: mode})
	}
	in := model.InfillRequest{Prefix: lead.String() + prefix, Suffix: suffix, Language: req.Language, MaxTokens: completeMaxTokens, Stop: []string{"\n\n\n"}}
	var res model.InfillResult
	var err error
	if fim {
		res, err = a.(model.Infiller).Infill(ctx, in)
	} else {
		res, err = model.InfillByChat(ctx, a, in)
	}
	k.mu.Lock()
	k.sum.Requests++
	k.sum.TokensIn += res.InputTokens
	k.sum.TokensOut += res.OutputTokens
	superseded := gen != k.gen
	switch {
	case err != nil && (errors.Is(ctx.Err(), context.Canceled) || superseded):
		k.sum.Cancelled++
	case err != nil:
		k.sum.Failed++
	case strings.TrimSpace(res.Text) != "":
		k.sum.Served++
	}
	k.mu.Unlock()
	switch {
	case err != nil && (errors.Is(ctx.Err(), context.Canceled) || superseded):
		empty("cancelled", "a newer completion replaced it")
	case err != nil:
		c.reply(msg.ID, nil, refusal(errRefused, "the completion model did not answer: %s", redacted(err.Error())))
	case strings.TrimSpace(res.Text) == "":
		c.reply(msg.ID, map[string]any{"items": []any{}}, nil)
	default:
		c.reply(msg.ID, map[string]any{"items": []map[string]any{{"text": res.Text}}}, nil)
	}
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
	k := c.completer(s.id)
	k.mu.Lock()
	if k.cancel != nil {
		k.cancel()
	}
	k.mu.Unlock()
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
	if k.cancel != nil {
		k.cancel()
	}
	sum := k.sum
	k.mu.Unlock()
	if sum.Requests+sum.Refused+sum.Cancelled == 0 {
		return
	}
	if s.inner {
		if _, err := s.parts.Loop.Recorder.Record(agent.EvCompletionSummary, agent.ActorSystem, agent.Trusted, sum); err != nil {
			warnf("the record did not take %s: %v", agent.EvCompletionSummary, err)
		}
	}
}
