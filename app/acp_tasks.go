package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// Orchestration (docs/architecture/studio-acp-contract.md §6): background
// tasks listed and stopped one by one, and the held asks a person may
// review without sending a prompt. Nothing is approved while idle.

func init() {
	liveFeatures = append(liveFeatures, "tasks", "tasks.review")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/tasks/list":   (*acpConn).listTasks,
		"_abhed/tasks/cancel": (*acpConn).cancelTask,
		"_abhed/tasks/review": (*acpConn).reviewTasks,
	})
}

// tasker is an agent with background tasks; the SDK's agent is one.
type tasker interface {
	Background() []abhed.TaskInfo
	CancelTask(id string) error
}

// taskNote is what the record says about a task beyond the SDK's TaskInfo.
type taskNote struct {
	branch           string
	tokensIn, tokOut int
	delivery         string
	contentChars     int
	noticed          bool
}

// liveTasks counts the session's background tasks still running.
func (s *acpSession) liveTasks() int {
	t, ok := s.agent.(tasker)
	if !ok {
		return 0
	}
	n := 0
	for _, ti := range t.Background() {
		if ti.Status == "running" {
			n++
		}
	}
	return n
}

// waitingAll counts the asks held for a person across the session's tasks.
func (s *acpSession) waitingAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.waiting {
		n += w
	}
	return n
}

// taskInfo is a TaskInfo as the contract shapes it.
func (s *acpSession) taskInfo(ti abhed.TaskInfo) map[string]any {
	s.mu.Lock()
	note := s.notes[ti.ID]
	waiting := s.waiting[ti.ID]
	s.mu.Unlock()
	out := map[string]any{"task_id": ti.ID, "description": ti.Description, "status": ti.Status,
		"started": ti.Started.UTC().Format(time.RFC3339), "waiting_asks": waiting}
	for k, v := range map[string]string{"agent_type": ti.AgentType, "provider": ti.Provider, "model": ti.Model,
		"reason": ti.Reason, "summary": ti.Summary, "branch": note.branch} {
		if v != "" {
			out[k] = v
		}
	}
	if ti.Turns > 0 {
		out["turns"] = ti.Turns
	}
	out["kind"] = ti.Kind
	if ti.Kind == agent.KindShell {
		for k, v := range map[string]string{"command": ti.Command, "last_line": ti.LastLine} {
			if v != "" {
				out[k] = v
			}
		}
		out["output_bytes"] = ti.OutputBytes
		if ti.ExitCode != nil {
			out["exit_code"] = *ti.ExitCode
		}
	}
	if note.noticed {
		out["tokens_in"], out["tokens_out"] = note.tokensIn, note.tokOut
		if note.delivery != "" {
			out["notice"] = map[string]any{"delivery": note.delivery, "content_chars": note.contentChars}
		}
	}
	return out
}

func (s *acpSession) task(id string) (abhed.TaskInfo, bool) {
	t, ok := s.agent.(tasker)
	if !ok {
		return abhed.TaskInfo{}, false
	}
	for _, ti := range t.Background() {
		if ti.ID == id {
			return ti, true
		}
	}
	return abhed.TaskInfo{}, false
}

// taskChanged tells Studio a task's status, turns or waiting asks moved.
func (c *acpConn) taskChanged(s *acpSession, id string) {
	if id == "" {
		return
	}
	if ti, ok := s.task(id); ok {
		c.notification("_abhed/tasks/changed", map[string]any{"sessionId": s.id, "task": s.taskInfo(ti)})
	}
}

// taskEvent keeps the task notes and says when a task changed.
func (c *acpConn) taskEvent(s *acpSession, ev abhed.Event) {
	switch ev.Type {
	case agent.EvSubagentSpawned:
		var p struct {
			Background bool   `json:"background"`
			TaskID     string `json:"task_id"`
			Branch     string `json:"branch"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || !p.Background || p.TaskID == "" {
			return
		}
		s.mu.Lock()
		if s.notes == nil {
			s.notes = map[string]taskNote{}
		}
		n := s.notes[p.TaskID]
		n.branch = p.Branch
		s.notes[p.TaskID] = n
		s.mu.Unlock()
		c.taskChanged(s, p.TaskID)
	case agent.EvShellStarted, agent.EvShellEnded:
		var p struct {
			ShellID string `json:"shell_id"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			c.taskChanged(s, p.ShellID)
		}
	case agent.EvSubagentNotice:
		var p agent.Notice
		if json.Unmarshal(ev.Payload, &p) != nil || p.TaskID == "" {
			return
		}
		s.mu.Lock()
		if s.notes == nil {
			s.notes = map[string]taskNote{}
		}
		n := s.notes[p.TaskID]
		n.tokensIn, n.tokOut, n.delivery, n.contentChars, n.noticed = p.TokensIn, p.TokensOut, p.Delivery, len(p.Content), true
		s.notes[p.TaskID] = n
		s.mu.Unlock()
		c.taskChanged(s, p.TaskID)
	}
}

func (c *acpConn) listTasks(msg rpcMessage) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	out := []any{}
	if t, ok := s.agent.(tasker); ok {
		for _, ti := range t.Background() {
			out = append(out, s.taskInfo(ti))
		}
	}
	c.reply(msg.ID, map[string]any{"tasks": out}, nil)
}

// cancelTask stops one task, as the person's stop, and records that they did.
func (c *acpConn) cancelTask(msg rpcMessage) {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	t, ok := s.agent.(tasker)
	if !ok {
		c.reply(msg.ID, nil, refusal(errRefused, "this session has no background tasks"))
		return
	}
	if _, known := s.task(p.TaskID); !known {
		c.reply(msg.ID, nil, refusal(errParams, "unknown task"))
		return
	}
	cancelled := t.CancelTask(p.TaskID) == nil
	if cancelled {
		s.record(agent.EvTaskCancelled, agent.TaskCancelled{TaskID: p.TaskID, By: agent.ByUser})
	}
	c.taskChanged(s, p.TaskID)
	c.reply(msg.ID, map[string]any{"cancelled": cancelled}, nil)
}

// reviewTasks opens a review window: the asks held for a task, or for all,
// go to the editor now as ordinary permission requests marked held (§6.3).
func (c *acpConn) reviewTasks(msg rpcMessage) {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if p.TaskID != "" {
		if _, known := s.task(p.TaskID); !known {
			c.reply(msg.ID, nil, refusal(errParams, "unknown task"))
			return
		}
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		// Inside a turn the asks already go out; there is nothing held to release.
		c.reply(msg.ID, map[string]any{"released": 0}, nil)
		return
	}
	released := 0
	for task, n := range s.waiting {
		if p.TaskID == "" || task == p.TaskID {
			released += n
		}
	}
	w := s.window
	if w == nil || !w.open {
		ctx, cancel := context.WithCancel(context.Background())
		w = &reviewWindow{ctx: ctx, cancel: cancel, open: true, tasks: map[string]bool{}}
		s.window = w
	}
	if p.TaskID == "" {
		w.all = true
	} else {
		w.tasks[p.TaskID] = true
	}
	if w.outstanding == 0 {
		s.lingerLocked(w)
	}
	s.pokeLocked()
	s.mu.Unlock()
	c.reply(msg.ID, map[string]any{"released": released}, nil)
}

// covers reports whether the window lets task's asks out.
func (w *reviewWindow) covers(task string) bool {
	return w.all || w.tasks[task]
}

// stopLinger keeps an answered window from closing while an ask is out.
func (w *reviewWindow) stopLinger() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// lingerLocked closes w a while after its last answer, unless another ask
// goes out first. The caller holds s.mu.
func (s *acpSession) lingerLocked(w *reviewWindow) {
	w.stopLinger()
	if !w.open {
		s.closeWindowLocked()
		return
	}
	w.timer = time.AfterFunc(reviewLinger, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.window == w && w.outstanding == 0 {
			s.closeWindowLocked()
		}
	})
}

// windowAnswered counts an ask the window let out as answered.
func (c *acpConn) windowAnswered(s *acpSession, w *reviewWindow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.outstanding--
	if s.window == w && w.outstanding == 0 {
		s.lingerLocked(w)
	}
}

// closeWindowLocked ends the review window: the asks it let out and still
// open are refused. The caller holds s.mu.
func (s *acpSession) closeWindowLocked() {
	w := s.window
	if w == nil {
		return
	}
	w.stopLinger()
	w.open = false
	w.cancel()
	s.window = nil
	s.pokeLocked()
}
