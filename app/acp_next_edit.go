package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/linediff"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/secretfiles"
)

// An editor's next-edit suggestion (studio-acp-contract.md §7.11): after a
// change, the lines around the cursor and the recent edits go to the
// next-edit model, which answers with the window rewritten; the engine turns
// that into edits inside the document. No tools, no side effects.

func init() {
	liveFeatures = append(liveFeatures, "nextEdit", "nextEditCancel")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/nextEdit":        (*acpConn).nextEdit,
		"_abhed/nextEdit/cancel": (*acpConn).nextEditCancel,
	})
}

// nextEditCancel ends the session's next-edit request in flight, as Studio
// asks on each keystroke: its window is stale.
func (c *acpConn) nextEditCancel(msg rpcMessage) { c.featureCancel(msg, "next_edit") }

// nextEditTokens bounds the answer by the window sent: the model writes the
// window back with one edit. Half the window's bytes in tokens (code runs
// near four bytes a token, dense or non-ASCII code nearer two) leaves room
// for a real hunk; an answer cut at the bound is dropped, never offered.
func nextEditTokens(window string) int {
	return min(nextEditMaxTokens, max(128, len(window)/2+64))
}

// Next-edit bounds: the window sent, the recent edits, and the answer.
const (
	nextEditLines      = 80
	nextEditWindow     = 8 << 10
	nextEditRecent     = 5
	nextEditRecentText = 1 << 10
	nextEditMaxTokens  = 1024
)

type editPos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type editRange struct {
	Start editPos `json:"start"`
	End   editPos `json:"end"`
}

type nextEditRequest struct {
	URI         string `json:"uri"`
	RecentEdits []struct {
		URI     string    `json:"uri"`
		Range   editRange `json:"range"`
		OldText string    `json:"oldText"`
		NewText string    `json:"newText"`
	} `json:"recentEdits"`
	Cursor editPos `json:"cursor"`
	// Window is the document's lines around the cursor, from StartLine.
	Window struct {
		StartLine int    `json:"startLine"`
		Text      string `json:"text"`
	} `json:"window"`
	Language string `json:"language"`
	Seq      int64  `json:"seq"`
	// Rename is Studio's reading of the last edit as a rename, and Sites
	// where its language server says the old name is used in this document.
	Rename *renameHint `json:"rename"`
	Sites  []editRange `json:"sites"`
}

// suggestedEdit is one edit as the answer carries it.
type suggestedEdit struct {
	URI     string    `json:"uri"`
	Range   editRange `json:"range"`
	NewText string    `json:"newText"`
}

func (c *acpConn) nextEdit(msg rpcMessage) {
	arrived := time.Now()
	var req nextEditRequest
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
		c.reply(msg.ID, map[string]any{"edits": []any{}, field: why}, nil)
	}
	cfg := parts.Config
	off, provider := editorOff(cfg, "next_edit")
	if off != "" {
		empty("disabled", off)
		return
	}
	k := c.completerFor(s, "next_edit")
	if k == nil {
		c.reply(msg.ID, nil, refusal(errParams, "the session has closed"))
		return
	}
	refuse := func(why string) {
		k.count(func(s *agent.CompletionSummary) { s.Refused++ })
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
	if req.Window.StartLine < 0 || strings.TrimSpace(req.Window.Text) == "" {
		empty("refused", "no window around the cursor was given")
		return
	}

	name, _ := cfg.EditorModel("next_edit")
	ctx, cancel, a, why, field := c.admit(k, req.Seq, name, provider, cfg.Completion.LocalOnly)
	if why != "" {
		c.admitRefused(msg, empty, why, field)
		return
	}
	defer cancel()
	if completeHold(ctx) != nil {
		k.count(func(s *agent.CompletionSummary) { s.Superseded++ })
		empty("cancelled", "a newer request replaced it")
		return
	}
	if !c.spend(k) {
		empty("limited", "too many requests; slow down")
		return
	}

	clean := func(text string) string {
		text = parts.Loop.Withheld(text)
		text, _ = secretfiles.RedactContent(text)
		return text
	}
	// The window is cleaned whole, so a shape that spans lines (a block
	// scalar) is caught, and then bounded by lines and bytes; lines the
	// cleaning changed may not be edited, since what the model saw is not
	// there. A cleaning that would not keep the lines apart is refused.
	raw := linediff.SplitLines(req.Window.Text)
	if len(raw) > nextEditLines {
		raw = raw[:nextEditLines]
	}
	// A rename is finished from the document itself: no model, nothing sent.
	// A .js file holding JSX is renamed as JSX is: only at a language server's sites.
	rreq := req
	rreq.Language = renameLanguage(req.Language, raw)
	if h, line, ok := renameOf(rreq, raw); ok {
		if edits, why, notRename := renameEdit(rreq, raw, h, line); !notRename {
			if len(edits) == 0 {
				k.count(func(s *agent.CompletionSummary) { s.LocalDropped++ })
				empty("dropped", why)
				return
			}
			// Offered like any suggestion: served, and of those, from the file.
			k.count(func(s *agent.CompletionSummary) { s.Local++; s.Served++ })
			c.reply(msg.ID, map[string]any{"edits": edits, "rename": true}, nil)
			return
		}
	}
	cleaned := linediff.SplitLines(clean(strings.Join(raw, "")))
	if len(cleaned) != len(raw) {
		refuse("the window holds a secret over several lines")
		return
	}
	var shown []string
	size := 0
	for _, l := range cleaned {
		if size+len(l) > nextEditWindow {
			break
		}
		size += len(l)
		shown = append(shown, l)
	}
	var recent strings.Builder
	n := 0
	for _, r := range req.RecentEdits {
		if n == nextEditRecent {
			break
		}
		rp, why := sendable(r.URI, roots)
		if why != "" || readDenied(parts.Loop.Policy, rp) {
			continue
		}
		rel := rootRel(rp, roots)
		fmt.Fprintf(&recent, "In %s at line %d:\n- %s\n+ %s\n", rel, r.Range.Start.Line+1,
			clean(headBytes(r.OldText, nextEditRecentText)), clean(headBytes(r.NewText, nextEditRecentText)))
		n++
	}
	if ctx.Err() != nil {
		k.count(func(s *agent.CompletionSummary) { s.Superseded++ })
		empty("cancelled", "a newer request replaced it")
		return
	}
	if why := editorRefused(parts.Loop, a, "next-edit suggestions", "next-edit"); why != "" {
		c.reply(msg.ID, k.priceNotice(map[string]any{"edits": []any{}}, why), nil)
		return
	}
	if why := c.completeEnabled(s, k, provider, "chat"); why != "" {
		refuse(why)
		return
	}
	rel := rootRel(path, roots)
	// Completions go first on a shared server: this call waits for them, and
	// one asked for while it runs cancels it.
	mctx, release, werr := c.pool.nextEditTurn(ctx, editorServer(provider))
	if werr != nil {
		k.count(func(s *agent.CompletionSummary) { s.Superseded++ })
		empty("cancelled", "a newer request replaced it")
		return
	}
	defer release()
	window := strings.Join(shown, "")
	capTokens := nextEditTokens(window)
	// A call ended while it waited for the server's request slot was never sent: nothing to charge.
	var queued, sent atomic.Bool
	mctx = model.WithSlotWait(mctx, func(_ model.SlotWait, done bool) {
		if done {
			sent.Store(true)
		} else {
			queued.Store(true)
		}
	})
	res, err := nextEditCall(mctx, a, model.NextEditRequest{
		File: rel, Language: req.Language, Recent: recent.String(),
		Window: window, CursorLine: req.Cursor.Line - req.Window.StartLine, MaxTokens: capTokens,
	})
	cancelled := err != nil && mctx.Err() != nil
	preempted := cancelled && errors.Is(context.Cause(mctx), errCompletionFirst)
	var tokIn, tokOut int
	var usd *float64
	var unpriced, estimated bool
	if err == nil || !queued.Load() || sent.Load() {
		tokIn, tokOut, usd, unpriced, estimated = editorCharge(parts.Loop, a, res.InputTokens, res.OutputTokens, err, cancelled,
			recent.Len()+len(window), len(res.Window), capTokens)
	}
	if err == nil {
		k.took(time.Since(arrived))
	}
	edits, dropped := []suggestedEdit{}, res.Dropped
	if err == nil && dropped == "" {
		edits, dropped = windowEdits(req.URI, req.Window.StartLine, raw[:len(shown)], shown, res.Window, clean)
	}
	k.count(func(sum *agent.CompletionSummary) {
		addCost(sum, usd, unpriced)
		sum.Requests++
		sum.TokensIn += tokIn
		sum.TokensOut += tokOut
		if estimated {
			sum.Estimated++
		}
		switch {
		case cancelled:
			sum.Cancelled++
		case err != nil:
			sum.Failed++
		case len(edits) > 0:
			sum.Served++
		}
	})
	switch {
	case preempted:
		empty("cancelled", "a completion went first")
	case cancelled:
		empty("cancelled", "a newer request replaced it")
	case err != nil:
		c.reply(msg.ID, nil, refusal(errRefused, "the next-edit model did not answer: %s", redacted(err.Error())))
	case len(edits) == 0 && dropped != "":
		empty("dropped", dropped)
	default:
		c.reply(msg.ID, map[string]any{"edits": edits}, nil)
	}
}

// windowEdits turns the model's rewrite of the window into edits of the
// document, by line. A changed line that held a withheld secret, or new text
// that carries one, is not offered: what the model saw there is not the file.
func windowEdits(uri string, start int, orig, shown []string, rewrite string, clean func(string) string) ([]suggestedEdit, string) {
	if clean(rewrite) != rewrite {
		return nil, "the suggestion carried a secret"
	}
	next := linediff.SplitLines(rewrite)
	// The model's rewrite takes the window's line ends: a line it copied keeps
	// the whitespace the person wrote at its end (a Markdown hard break), a
	// line it changed loses any it added, every line ends as the window's do
	// (CRLF or LF), and the last newline is there or not as in the window.
	crlf := len(shown) > 0 && strings.HasSuffix(shown[0], "\r\n")
	eol := "\n"
	if crlf {
		eol = "\r\n"
	}
	asWritten := map[string]string{}
	for _, l := range shown {
		body := strings.TrimRight(l, "\r\n")
		key := strings.TrimRight(body, " \t")
		if _, seen := asWritten[key]; !seen && key != "" { // a blank line's spaces are given to no other
			asWritten[key] = body
		}
	}
	for i, l := range next {
		body, nl := strings.CutSuffix(l, "\n")
		body = strings.TrimRight(strings.TrimSuffix(body, "\r"), " \t")
		if orig, ok := asWritten[body]; ok {
			body = orig
		}
		next[i] = body
		if nl {
			next[i] += eol
		}
	}
	if n := len(next); n > 0 && len(shown) > 0 {
		last := strings.HasSuffix(shown[len(shown)-1], "\n")
		if strings.HasSuffix(next[n-1], "\n") != last {
			if last {
				next[n-1] += eol
			} else {
				next[n-1] = strings.TrimSuffix(strings.TrimSuffix(next[n-1], "\n"), "\r")
			}
		}
	}
	edits := []suggestedEdit{}
	skipped := false
	hunks := linediff.Hunks(shown, next)
	changed := 0
	// A change of trailing whitespace alone (often what is left where the
	// model's cursor marker was) leaves the line looking the same: no edit.
	var real []linediff.Hunk
	for _, h := range hunks {
		if !sameBarTrailingSpace(shown[h.OldStart:h.OldStart+h.OldCount], next[h.NewStart:h.NewStart+h.NewCount]) {
			real = append(real, h)
		}
	}
	hunks = real
	for _, h := range hunks {
		changed += max(h.OldCount, h.NewCount)
	}
	// A next edit is small; a window rewritten wholesale is not one.
	if len(shown) > 4 && changed > len(shown)/2 {
		return edits, "the suggestion rewrote most of the window"
	}
	for _, h := range hunks {
		safe := true
		for i := h.OldStart; i < h.OldStart+h.OldCount; i++ {
			if shown[i] != orig[i] {
				safe = false
			}
		}
		if !safe {
			skipped = true
			continue
		}
		end := editPos{Line: start + h.OldStart + h.OldCount}
		if last := h.OldStart + h.OldCount; last == len(shown) && last > 0 && !strings.HasSuffix(shown[last-1], "\n") {
			// The window's end has no newline after it: the range ends at the end of its last line.
			end = editPos{Line: start + last - 1, Character: len(utf16.Encode([]rune(shown[last-1])))}
		}
		edits = append(edits, suggestedEdit{
			URI: uri,
			Range: editRange{
				Start: editPos{Line: start + h.OldStart},
				End:   end,
			},
			NewText: strings.Join(next[h.NewStart:h.NewStart+h.NewCount], ""),
		})
	}
	if len(edits) == 0 && skipped {
		return edits, "the only change touched a line holding a secret"
	}
	if len(edits) == 0 {
		return edits, "the model suggested no change"
	}
	return edits, ""
}

// rootRel is path relative to the root that holds it, as the model is shown it.
func rootRel(path string, roots []string) string {
	for _, r := range roots {
		if rel, err := filepath.Rel(r, path); err == nil && filepath.IsLocal(rel) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
}

// sameBarTrailingSpace reports whether two runs of lines differ only in the
// whitespace at the ends of their lines. Line endings count as that
// whitespace, so a suggestion that only adds the window's missing final
// newline, or turns CRLF into LF, is dropped as no change too.
func sameBarTrailingSpace(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimRight(a[i], " \t\r\n") != strings.TrimRight(b[i], " \t\r\n") {
			return false
		}
	}
	return true
}

// nextEditCall is next-edit's one model call (a rename finished from the
// file makes none), kept apart so what every model call passes through sits
// in one place.
func nextEditCall(ctx context.Context, a model.Adapter, req model.NextEditRequest) (model.NextEditResult, error) {
	return model.NextEditByChat(ctx, a, req)
}
