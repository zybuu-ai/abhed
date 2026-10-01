package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/embedded"
	"github.com/zybuu-ai/abhed/internal/linediff"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/store/local"
)

// The person's own actions (docs/architecture/studio-acp-contract.md §7): the
// sandboxed Abhed terminal, saves and unsaved buffers, per-hunk review, undo,
// and steering. Each goes through the engine's manual path and is recorded
// by: user; none is a way for Studio to act for the agent.

func init() {
	liveFeatures = append(liveFeatures, "terminal", "manual", "review", "checkpoints", "queue")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/terminal/create": (*acpConn).terminalCreate,
		"_abhed/terminal/input":  (*acpConn).terminalInput,
		"_abhed/terminal/resize": (*acpConn).terminalResize,
		"_abhed/terminal/kill":   (*acpConn).terminalKill,
		"_abhed/manual/edited":   (*acpConn).manualEdited,
		"_abhed/buffers/dirty":   (*acpConn).buffersDirty,
		"_abhed/review/list":     (*acpConn).reviewList,
		"_abhed/review/baseline": (*acpConn).reviewBaseline,
		"_abhed/review/accept":   func(c *acpConn, m rpcMessage) { c.reviewDecide(m, true) },
		"_abhed/review/reject":   func(c *acpConn, m rpcMessage) { c.reviewDecide(m, false) },
		"_abhed/review/undoTurn": (*acpConn).undoTurn,
		"_abhed/session/steer":   (*acpConn).steer,
		"_abhed/queue/list":      (*acpConn).queueList,
		"_abhed/queue/cancel":    (*acpConn).queueCancel,
	})
}

// maxTerminalLine bounds one line entered at the Abhed terminal.
const maxTerminalLine = 8 << 10

// maxTerminals bounds the Abhed terminals one connection keeps open.
const maxTerminals = 8

// acpTerminal is one sandboxed terminal in lines mode: each line the person
// enters is a bash call put to policy and run in the session's sandbox, on a
// tool session of its own so a cd there never moves the agent.
type acpTerminal struct {
	id      string
	session *acpSession
	sess    *tools.Session
	mu      sync.Mutex
	line    []byte
	queue   chan string
	ctx     context.Context
	cancel  context.CancelFunc
	// running cancels the command running now; nil when none is.
	running context.CancelFunc
}

func (c *acpConn) terminalCreate(msg rpcMessage) {
	var p struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(msg.Params, &p)
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
	switch p.Mode {
	case "lines":
	case "interactive":
		c.reply(msg.ID, nil, refusal(errRefused, "an interactive Abhed terminal is not available over ACP in this engine yet; open one in lines mode"))
		return
	default:
		c.reply(msg.ID, nil, refusal(errParams, `mode is "lines" or "interactive"`))
		return
	}
	bash, ok := parts.Loop.Tools.Get("bash")
	if !ok {
		c.reply(msg.ID, nil, refusal(errRefused, "this session has no shell"))
		return
	}
	iso := tools.Isolation{Tier: "none"}
	if b, ok := bash.(tools.Bash); ok && b.Isolation.Tier != "" {
		iso = b.Isolation
	}
	c.termMu.Lock()
	if c.terms == nil {
		c.terms = map[string]*acpTerminal{}
	}
	if len(c.terms) >= maxTerminals {
		c.termMu.Unlock()
		c.reply(msg.ID, nil, refusal(errRefused, "%d Abhed terminals are open; close one first", maxTerminals))
		return
	}
	ctx, cancel := context.WithCancel(c.root())
	t := &acpTerminal{id: "term-" + acpID(), session: s, sess: parts.Session.Fork(), queue: make(chan string, 16), ctx: ctx, cancel: cancel}
	c.terms[t.id] = t
	c.termMu.Unlock()
	go c.runTerminal(t)
	c.reply(msg.ID, map[string]any{"terminalId": t.id, "tier": iso.Tier, "network": iso.Network, "recorded": true}, nil)
}

func (c *acpConn) terminal(params json.RawMessage) (*acpTerminal, *rpcError) {
	var p struct {
		TerminalID string `json:"terminalId"`
	}
	_ = json.Unmarshal(params, &p)
	c.termMu.Lock()
	defer c.termMu.Unlock()
	t := c.terms[p.TerminalID]
	if t == nil {
		return nil, refusal(errParams, "unknown terminal")
	}
	return t, nil
}

// output sends bytes to the terminal's view, base64 as the contract says.
func (c *acpConn) output(t *acpTerminal, b []byte) {
	if len(b) == 0 {
		return
	}
	c.notification("_abhed/terminal/output", map[string]any{"terminalId": t.id, "data": base64.StdEncoding.EncodeToString(b)})
}

func (c *acpConn) terminalInput(msg rpcMessage) {
	var p struct {
		Data string `json:"data"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	t, e := c.terminal(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	var lines []string
	var echo []byte
	t.mu.Lock()
	for _, b := range []byte(p.Data) {
		switch {
		case b == 0x03: // Ctrl-C stops the command running and drops the line
			if t.running != nil {
				t.running()
			}
			t.line = t.line[:0]
			echo = append(echo, "^C\r\n"...)
		case b == '\r' || b == '\n':
			lines = append(lines, string(t.line))
			t.line = t.line[:0]
			echo = append(echo, "\r\n"...)
		case b == 0x7f || b == 0x08:
			if len(t.line) > 0 {
				t.line = t.line[:len(t.line)-1]
				echo = append(echo, "\b \b"...)
			}
		case b < 0x20:
		default:
			if len(t.line) < maxTerminalLine {
				t.line = append(t.line, b)
				echo = append(echo, b)
			}
		}
	}
	t.mu.Unlock()
	c.output(t, echo)
	for _, l := range lines {
		select {
		case t.queue <- l:
		default:
			c.output(t, []byte("abhed: too many lines waiting; this one was not run\r\n"))
		}
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

// runTerminal runs the terminal's lines one at a time until it is killed.
func (c *acpConn) runTerminal(t *acpTerminal) {
	defer func() {
		c.notification("_abhed/terminal/exit", map[string]any{"terminalId": t.id, "code": 0})
	}()
	for {
		select {
		case <-t.ctx.Done():
			return
		case line := <-t.queue:
			if line == "" {
				continue
			}
			c.runLine(t, line)
		}
	}
}

// runLine is one entered line: a bash call put to policy as the person's,
// a destructive one confirmed in Studio's modal, run in the sandbox, recorded.
func (c *acpConn) runLine(t *acpTerminal, line string) {
	loop := t.session.parts.Loop
	id := "u" + acpID()
	args, _ := json.Marshal(map[string]string{"command": line, "description": "entered in the Abhed terminal"})
	tool, refused, confirm, err := loop.ManualAuthorizeTyped(id, args, agent.Unanswered)
	if err == nil && confirm != "" {
		answer := agent.Declined
		res, cerr := c.call(t.ctx, "_abhed/terminal/confirm", map[string]any{"terminalId": t.id, "command": ui.VisibleLine(line), "reason": confirm})
		var out struct {
			Confirmed bool `json:"confirmed"`
		}
		if cerr == nil && json.Unmarshal(res, &out) == nil && out.Confirmed {
			answer = agent.Confirmed
		}
		tool, refused, _, err = loop.ManualAuthorizeTyped(id, args, answer)
	}
	if err != nil {
		c.output(t, []byte("abhed: not run: "+err.Error()+"\r\n"))
		return
	}
	if refused != nil {
		_ = loop.ManualObserve(id, "bash", *refused, 0)
		c.output(t, crlf(refused.Content))
		return
	}
	ctx, cancel := context.WithCancel(t.ctx)
	t.mu.Lock()
	t.running = cancel
	t.mu.Unlock()
	start := time.Now()
	result := tool.Run(ctx, t.sess, args)
	cancel()
	t.mu.Lock()
	t.running = nil
	t.mu.Unlock()
	result.Content = redacted(result.Content)
	_ = loop.ManualObserve(id, "bash", result, time.Since(start))
	c.output(t, crlf(result.Content))
}

// crlf ends each line as a terminal expects.
func crlf(s string) []byte {
	b := bytes.ReplaceAll([]byte(s), []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\r\n")) {
		b = append(b, "\r\n"...)
	}
	return b
}

func (c *acpConn) terminalResize(msg rpcMessage) {
	// Lines mode draws no screen of its own; the size is the view's.
	if _, e := c.terminal(msg.Params); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) terminalKill(msg rpcMessage) {
	t, e := c.terminal(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.dropTerminal(t)
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) dropTerminal(t *acpTerminal) {
	c.termMu.Lock()
	delete(c.terms, t.id)
	c.termMu.Unlock()
	t.mu.Lock()
	if t.running != nil {
		t.running()
	}
	t.mu.Unlock()
	t.cancel()
}

// killTerminals ends a session's terminals, or every one for "".
func (c *acpConn) killTerminals(sessionID string) {
	c.termMu.Lock()
	var mine []*acpTerminal
	for _, t := range c.terms {
		if sessionID == "" || t.session.id == sessionID {
			mine = append(mine, t)
		}
	}
	c.termMu.Unlock()
	for _, t := range mine {
		c.dropTerminal(t)
	}
}

// within canonicalises path and reports whether it lies in one of the
// session's roots; anything else is dropped (§7.3).
func (s *acpSession) within(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.cwd, path)
	}
	real := tools.RealPath(filepath.Clean(path))
	for _, root := range s.roots() {
		rel, err := filepath.Rel(tools.RealPath(root), real)
		if err == nil && filepath.IsLocal(rel) {
			return real, true
		}
	}
	return "", false
}

// maxPatch bounds the patch a manual edit records.
const maxPatch = 1 << 20

// manualEdited records a person's own save. It is not judged: it is their
// file on their machine. The record says they made it.
func (c *acpConn) manualEdited(msg rpcMessage) {
	var p struct {
		Path   string `json:"path"`
		Before string `json:"beforeSha256"`
		After  string `json:"afterSha256"`
		Patch  string `json:"patch"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	path, ok := s.within(p.Path)
	if !ok {
		c.reply(msg.ID, map[string]any{}, nil)
		return
	}
	if !isSHA(p.Before) || !isSHA(p.After) {
		c.reply(msg.ID, nil, refusal(errParams, "beforeSha256 and afterSha256 are SHA-256 hashes in hex, or empty"))
		return
	}
	if len(p.Patch) > maxPatch {
		p.Patch = ""
	}
	s.record(agent.EvManualEdit, agent.ManualEdit{Path: path, BeforeSHA256: p.Before, AfterSHA256: p.After, Patch: p.Patch, By: agent.ByUser})
	c.reply(msg.ID, map[string]any{}, nil)
}

func isSHA(s string) bool {
	if s == "" {
		return true
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

// buffersDirty is the set of files with unsaved changes in Studio; the agent's
// edit or write to one of them is refused until it is saved.
func (c *acpConn) buffersDirty(msg rpcMessage) {
	var p struct {
		Paths []string `json:"paths"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	dirty := map[string]bool{}
	for _, raw := range p.Paths {
		if path, ok := s.within(raw); ok {
			dirty[path] = true
		}
	}
	s.mu.Lock()
	s.dirty = dirty
	s.mu.Unlock()
	c.reply(msg.ID, map[string]any{}, nil)
}

// reviewFile is one file the agent changed, compared with its baseline.
type reviewFile struct {
	path            string
	before, after   []string
	existed, exists bool
	hunks           []linediff.Hunk
}

func (s *acpSession) reviewOf(path string) (reviewFile, bool) {
	before, existed, known := s.undo.Original(path)
	if !known {
		return reviewFile{}, false
	}
	after, err := s.parts.Session.ReadFile(path)
	exists := err == nil
	if !exists && !existed {
		return reviewFile{}, false
	}
	f := reviewFile{path: path, before: linediff.SplitLines(string(before)), after: linediff.SplitLines(string(after)),
		existed: existed, exists: exists}
	f.hunks = linediff.Hunks(f.before, f.after)
	return f, true
}

func (f reviewFile) status() string {
	switch {
	case !f.existed:
		return "added"
	case !f.exists:
		return "deleted"
	}
	return "modified"
}

func (c *acpConn) reviewList(msg rpcMessage) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if _, e := innerOf(s); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	files := []any{}
	for _, path := range s.undo.Changed() {
		f, ok := s.reviewOf(path)
		if !ok || (f.existed == f.exists && len(f.hunks) == 0) {
			continue
		}
		files = append(files, map[string]any{"path": path, "status": f.status(), "hunks": len(f.hunks)})
	}
	c.reply(msg.ID, map[string]any{"files": files}, nil)
}

func (c *acpConn) reviewBaseline(msg rpcMessage) {
	var p struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if _, e := innerOf(s); e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	path, ok := s.within(p.Path)
	if !ok {
		c.reply(msg.ID, nil, refusal(errParams, "no recorded change to this file"))
		return
	}
	before, _, known := s.undo.Original(path)
	if !known {
		c.reply(msg.ID, nil, refusal(errParams, "no recorded change to this file"))
		return
	}
	sum := sha256.Sum256(before)
	c.reply(msg.ID, map[string]any{"text": redacted(string(before)), "sha256": hex.EncodeToString(sum[:])}, nil)
}

// reviewDecide keeps (accept) or undoes (reject) a file's change, or one hunk
// of it. Accepting moves the baseline and is recorded change.accepted;
// rejecting writes the baseline back as the person's write, through policy.
func (c *acpConn) reviewDecide(msg rpcMessage, accept bool) {
	var p struct {
		Path string `json:"path"`
		Hunk *int   `json:"hunk"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e == nil {
		e = idle(s)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	path, ok := s.within(p.Path)
	f, known := s.reviewOf(path)
	if !ok || !known {
		c.reply(msg.ID, nil, refusal(errParams, "no recorded change to this file"))
		return
	}
	if p.Hunk != nil && (*p.Hunk < 0 || *p.Hunk >= len(f.hunks)) {
		c.reply(msg.ID, nil, refusal(errParams, "no hunk %d in this file", *p.Hunk))
		return
	}
	payload := map[string]any{"path": path, "by": agent.ByUser}
	if p.Hunk != nil {
		payload["hunk"] = *p.Hunk
	}
	if accept {
		next := joinLines(f.after)
		if p.Hunk != nil {
			next = linediff.TakeNew(f.before, f.after, f.hunks[*p.Hunk])
		}
		if !s.undo.Accept(path, []byte(next)) {
			c.reply(msg.ID, nil, refusal(errParams, "no recorded change to this file"))
			return
		}
		s.record(agent.EvChangeAccepted, payload)
	} else {
		// A protected file stays protected, for the person's reject as for the agent.
		if editorFile(path, s.roots()) {
			c.reply(msg.ID, nil, refusal(errPolicy, "%s", errEditorFile.Error()))
			return
		}
		if p.Hunk == nil && !f.existed {
			if err := s.manualRemove(c.root(), path); err != nil {
				c.reply(msg.ID, nil, refusal(errPolicy, "%v", err))
				return
			}
		} else {
			text := joinLines(f.before)
			if p.Hunk != nil {
				text = linediff.TakeOld(f.before, f.after, f.hunks[*p.Hunk])
			}
			if err := s.manualWrite(c.root(), parts, path, text); err != nil {
				c.reply(msg.ID, nil, refusal(errPolicy, "%v", err))
				return
			}
		}
	}
	remaining := 0
	if g, ok := s.reviewOf(path); ok {
		remaining = len(g.hunks)
	}
	c.notification("_abhed/review/changed", map[string]any{"sessionId": s.id, "paths": []string{path}})
	c.reply(msg.ID, map[string]any{"remaining": remaining}, nil)
}

func joinLines(lines []string) string {
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
	}
	return b.String()
}

// manualWrite writes a file as the person, through policy, on a tool session
// of their own, and recorded by: user.
func (s *acpSession) manualWrite(ctx context.Context, parts embedded.Parts, path, text string) error {
	sess := parts.Session.Fork()
	if sess.Syntax == tools.SyntaxRefuse {
		sess.Syntax = tools.SyntaxReport // a person's restore is warned about, never refused
	}
	if data, err := sess.ReadFile(path); err == nil {
		sess.MarkRead(path, string(data))
	}
	args, _ := json.Marshal(map[string]string{"path": path, "content": text})
	res, err := parts.Loop.Manual(ctx, sess, "write", "u"+acpID(), args)
	if err != nil {
		return err
	}
	if res.IsError {
		return errors.New(res.Content)
	}
	return nil
}

// manualRemove deletes a file the agent created, as the person's restore.
func (s *acpSession) manualRemove(_ context.Context, path string) error {
	before, _ := s.parts.Session.ReadFile(path)
	if err := s.parts.Session.RemoveFile(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.record(agent.EvFileRestored, agent.FileRestored{Path: path, BeforeSHA256: hashHex(before), Checkpoint: "baseline", By: agent.ByUser})
	return nil
}

// undoTurn puts back the files the agent changed in a turn and every turn
// after it, each through the manual path and recorded file.restored by: user.
// The pre-images are in the record's blobs, so this works after a restart.
// A turn is numbered as the record's checkpoint.saved events number it.
func (c *acpConn) undoTurn(msg rpcMessage) {
	var p struct {
		Turn int `json:"turn"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if _, e = innerOf(s); e == nil {
		e = idle(s)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if _, ok := s.turnSeq(p.Turn); !ok {
		c.reply(msg.ID, nil, refusal(errParams, "no turn %d with changes to undo", p.Turn))
		return
	}
	restored, err := c.restoreTurn(s, p.Turn)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the files could not all be restored: %v", err))
		return
	}
	c.reply(msg.ID, map[string]any{"restored": restored}, nil)
}

// restoreFrom puts back what turn and the turns after it changed, and names the files.
func (c *acpConn) restoreFrom(s *acpSession, turn int) ([]string, error) {
	restored, err := c.restoreTurn(s, turn)
	var paths []string
	for _, r := range restored {
		paths = append(paths, r.(map[string]any)["path"].(string))
	}
	return paths, err
}

func (c *acpConn) restoreTurn(s *acpSession, turn int) ([]any, error) {
	parts := s.parts
	since, ok := s.turnSeq(turn)
	if !ok {
		return []any{}, nil
	}
	cps := s.undo.Since(since)
	before := map[string]string{}
	for _, cp := range cps {
		if data, err := parts.Session.ReadFile(cp.Path); err == nil {
			before[cp.Path] = hashHex(data)
		}
	}
	current := func(path string) ([]byte, bool) {
		data, err := parts.Session.ReadFile(path)
		return data, err == nil
	}
	restore := func(path string, data []byte, existed bool, mode os.FileMode) error {
		if editorFile(path, s.roots()) {
			return errEditorFile
		}
		if !existed {
			if err := parts.Session.RemoveFile(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		return parts.Session.RestoreFileMode(path, data, mode)
	}
	var blobs agent.BlobPutter
	if rec, ok := parts.Store.(*local.Store); ok {
		blobs = rec.Blobs()
	}
	lines, err := parts.Loop.RestoreCheckpoints(cps, current, restore, blobs)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range lines {
		paths = append(paths, strings.TrimPrefix(strings.TrimPrefix(l, "restored "), "removed "))
	}
	sort.Strings(paths)
	out := []any{}
	for _, path := range paths {
		after := ""
		if data, err := parts.Session.ReadFile(path); err == nil {
			after = hashHex(data)
		}
		out = append(out, map[string]any{"path": path, "beforeSha256": before[path], "afterSha256": after})
	}
	if len(paths) > 0 {
		c.notification("_abhed/review/changed", map[string]any{"sessionId": s.id, "paths": paths})
	}
	return out, nil
}

// turnSeq is the seq just before the first checkpoint of turn, as the
// record's checkpoint.saved events number turns.
func (s *acpSession) turnSeq(turn int) (int64, bool) {
	for _, cp := range s.undo.Checkpoints() {
		if cp.Turn >= turn && cp.Seq > 0 {
			return cp.Seq - 1, true
		}
	}
	return 0, false
}

// steer sends a message into the running prompt, applied at its next turn
// boundary and recorded as the person's, marked steered.
func (c *acpConn) steer(msg rpcMessage) {
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(msg.Params, &p)
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
	if idle(s) == nil {
		c.reply(msg.ID, nil, refusal(errRefused, "no prompt is running; send it as a prompt"))
		return
	}
	if parts.Loop.QueueMessage(agent.Message{Text: p.Text, Steered: true}) == "" {
		c.reply(msg.ID, nil, refusal(errParams, "the message is empty"))
		return
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

func (c *acpConn) queueList(msg rpcMessage) {
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
	items := []any{}
	for _, q := range parts.Loop.Queued() {
		items = append(items, map[string]any{"id": q.ID, "text": q.Text, "queued": q.At.UTC().Format(time.RFC3339)})
	}
	c.reply(msg.ID, map[string]any{"items": items}, nil)
}

func (c *acpConn) queueCancel(msg rpcMessage) {
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(msg.Params, &p)
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
	if !parts.Loop.Unqueue(p.ID) {
		c.reply(msg.ID, nil, refusal(errParams, "no waiting message %q; it may have been delivered", ui.VisibleLine(p.ID)))
		return
	}
	c.reply(msg.ID, map[string]any{}, nil)
}
