package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
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
	liveFeatures = append(liveFeatures, "nextEdit")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/nextEdit": (*acpConn).nextEdit,
	})
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
}

// suggestedEdit is one edit as the answer carries it.
type suggestedEdit struct {
	URI     string    `json:"uri"`
	Range   editRange `json:"range"`
	NewText string    `json:"newText"`
}

func (c *acpConn) nextEdit(msg rpcMessage) {
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

	clean := func(text string) string {
		text = parts.Loop.Withheld(text)
		text, _ = secretfiles.RedactContent(text)
		return text
	}
	// The window is cleaned whole, then bounded by lines and bytes; lines the
	// cleaning changed may not be edited, since what the model saw is not there.
	raw := linediff.SplitLines(req.Window.Text)
	if len(raw) > nextEditLines {
		raw = raw[:nextEditLines]
	}
	var shown []string
	size := 0
	for _, l := range raw {
		l = clean(l)
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
	if why := c.completeEnabled(s, k, provider, "chat"); why != "" {
		refuse(why)
		return
	}
	rel := rootRel(path, roots)
	res, err := model.NextEditByChat(ctx, a, model.NextEditRequest{
		File: rel, Language: req.Language, Recent: recent.String(),
		Window: strings.Join(shown, ""), CursorLine: req.Cursor.Line - req.Window.StartLine, MaxTokens: nextEditMaxTokens,
	})
	cancelled := err != nil && ctx.Err() != nil
	edits, dropped := []suggestedEdit{}, res.Dropped
	if err == nil && dropped == "" {
		edits, dropped = windowEdits(req.URI, req.Window.StartLine, raw[:len(shown)], shown, res.Window, clean)
	}
	k.count(func(sum *agent.CompletionSummary) {
		sum.Requests++
		sum.TokensIn += res.InputTokens
		sum.TokensOut += res.OutputTokens
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
	// The model's rewrite takes the window's line ends: no spaces it added at
	// a line's end, and the window's own last newline, there or not.
	for i, l := range next {
		body, nl := strings.CutSuffix(l, "\n")
		next[i] = strings.TrimRight(body, " \t")
		if nl {
			next[i] += "\n"
		}
	}
	if n := len(next); n > 0 && len(shown) > 0 {
		last := strings.HasSuffix(shown[len(shown)-1], "\n")
		if strings.HasSuffix(next[n-1], "\n") != last {
			if last {
				next[n-1] += "\n"
			} else {
				next[n-1] = strings.TrimSuffix(next[n-1], "\n")
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
