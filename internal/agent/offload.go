package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/model"
)

// Offloading shrinks the window without losing anything. An old, large tool
// result is replaced in the conversation by a short stub that says what it was
// and how to get it back; the full text is already in the event record, and
// the recall tool reads it from there.
//
// It runs before compaction and well below its threshold. Compaction rewrites
// the whole history through the model and keeps only what the summary thought
// mattered; this is mechanical, costs no model call, and leaves every user
// message, every agent message and the shape of the work in place. A session
// that offloads well may never need to compact.
type Offloader struct {
	// Threshold is the fraction of the window at which offloading starts.
	Threshold float64
	// KeepRecent tool results are never touched: they are the working state.
	KeepRecent int
	// MinChars is the smallest result worth replacing. A stub costs about a
	// hundred tokens, so trading away less than that saves nothing.
	MinChars int
}

const (
	offloadMark = "[offloaded to the session record"
	stubHead    = 240
)

func NewOffloader(threshold float64) *Offloader {
	return &Offloader{Threshold: threshold, KeepRecent: 4, MinChars: 2000}
}

// Offloaded is the record of one pass, for the event log and HawkEYE.
type Offloaded struct {
	Results      int      `json:"results"`
	CharsBefore  int      `json:"chars_before"`
	CharsAfter   int      `json:"chars_after"`
	BeforeTokens int      `json:"before_tokens"`
	AfterTokens  int      `json:"after_tokens"`
	CallIDs      []string `json:"call_ids"`
}

// offloadIfNeeded stubs old tool results once the window passes the threshold.
//
// All eligible results go in one pass rather than one per turn. Rewriting an
// old message invalidates the endpoint's prefix cache from that point on, so
// doing it once per threshold crossing pays that cost once; trimming a little
// every turn would pay it every turn.
func (l *Loop) offloadIfNeeded() {
	o := l.Offloader
	if o == nil || o.Threshold <= 0 || l.Adapter == nil {
		return
	}
	used, window := l.contextSize()
	if window == 0 || float64(used) < o.Threshold*float64(window) {
		return
	}

	recent := 0
	info := Offloaded{BeforeTokens: used}
	for i := len(l.messages) - 1; i >= 0; i-- {
		m := &l.messages[i]
		if m.Role != model.RoleTool {
			continue
		}
		if recent < o.KeepRecent {
			recent++
			continue
		}
		if len(m.Content) < o.MinChars || strings.HasPrefix(m.Content, offloadMark) || len(m.Blocks) > 0 {
			continue
		}
		stub := offloadStub(m.ToolCallID, m.Content, l.toolFor(m.ToolCallID))
		info.Results++
		info.CharsBefore += len(m.Content)
		info.CharsAfter += len(stub)
		info.CallIDs = append(info.CallIDs, m.ToolCallID)
		m.Content = stub
	}
	if info.Results == 0 {
		return
	}
	info.AfterTokens, _ = l.contextSize()
	l.record(EvContextOffloaded, ActorSystem, info)
}

// toolFor names the call a result answers, so the stub can say what it held.
func (l *Loop) toolFor(callID string) string {
	for i := range l.messages {
		for _, c := range l.messages[i].ToolCalls {
			if c.ID == callID {
				return c.Name + " " + clipRunes(oneLine(string(c.Args)), 120)
			}
		}
	}
	return "a tool call"
}

func offloadStub(callID, content, what string) string {
	return fmt.Sprintf("%s: the result of %s — %d characters. It began:\n%s\n"+
		"Nothing was lost. If you need it again, call recall with call_id %q, "+
		"or recall with a query to search this session.]",
		offloadMark, what, len(content), clipRunes(content, stubHead), callID)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clipRunes cuts at a character boundary, never inside one.
func clipRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
