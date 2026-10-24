package app

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ollama"
	"github.com/zybuu-ai/abhed/internal/toolset"
)

// A local Ollama model chosen in Studio (studio-acp-contract.md §3.2a).
//
// Studio asks the person before every start, install and pull, in its own
// modal, and sends the method only after a click; the engine still applies
// model.ollama.assist, refuses inside an agent's command, acts only on this
// machine's Ollama, and records every attempt in the session's record.

func init() {
	liveFeatures = append(liveFeatures, "ollama")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/ollama/status":  (*acpConn).ollamaStatus,
		"_abhed/ollama/start":   (*acpConn).ollamaStart,
		"_abhed/ollama/install": (*acpConn).ollamaInstall,
		"_abhed/ollama/pull":    (*acpConn).ollamaPull,
		"_abhed/ollama/cancel":  (*acpConn).ollamaCancel,
		"_abhed/ollama/forget":  (*acpConn).ollamaForget,
	})
}

// ollamaParams are what every _abhed/ollama method takes.
type ollamaParams struct {
	SessionID string `json:"sessionId"`
	// Provider is a configured name; the session's current model when empty.
	Provider string `json:"provider"`
	// Always, on start, is the person's "always start without asking".
	Always bool `json:"always"`
	// PullID names a pull, for its progress and _abhed/ollama/cancel.
	PullID string `json:"pullId"`
}

// ollamaTarget finds the session and the Ollama target its request names.
func (c *acpConn) ollamaTarget(msg rpcMessage) (*acpSession, ollamaParams, ollama.Assist, ollama.Target, *rpcError) {
	var p ollamaParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, p, ollama.Assist{}, ollama.Target{}, badParams("params", err)
	}
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		return nil, p, ollama.Assist{}, ollama.Target{}, e
	}
	parts, e := innerOf(s)
	if e != nil {
		return nil, p, ollama.Assist{}, ollama.Target{}, e
	}
	cfg := parts.Config
	if p.Provider == "" {
		if m, ok := s.agent.(modelSwitcher); ok {
			p.Provider = currentModel(m.Models())
		}
		if p.Provider == "" {
			p.Provider = cfg.Model.Default
		}
	}
	if cfg.ModelAllowed(p.Provider) != nil {
		return nil, p, ollama.Assist{}, ollama.Target{}, refusal(errParams, "no model named %q is configured for this session", p.Provider)
	}
	t, ok := toolset.OllamaTarget(cfg, p.Provider)
	if !ok {
		return nil, p, ollama.Assist{}, ollama.Target{}, refusal(errParams, "%s is not served by Ollama", p.Provider)
	}
	a := toolset.OllamaAssist(cfg, ollamaEnv(), "acp", true, inAgentCommand(), func(t agent.EventType, ev agent.OllamaAction) {
		s.record(t, ev)
	})
	return s, p, a, t, nil
}

// ollamaRefusal is the error for a Problem: -32001 when policy or this
// machine forbids the step, -32000 when it failed; data carries the
// problem, with its numbered steps, and the status now.
func ollamaRefusal(p *ollama.Problem, st ollama.Status) *rpcError {
	code := errRefused
	switch p.Kind {
	case ollama.KindAssistOff, ollama.KindNotAdmin, ollama.KindInAgent, ollama.KindRemote:
		code = errPolicy
	}
	return &rpcError{Code: code, Message: p.Message, Data: map[string]any{"problem": p, "status": st}}
}

func (c *acpConn) ollamaStatus(msg rpcMessage) {
	_, _, a, t, e := c.ollamaTarget(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	c.reply(msg.ID, a.Report(c.root(), t), nil)
}

func (c *acpConn) ollamaStart(msg rpcMessage) {
	_, p, a, t, e := c.ollamaTarget(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	st := a.Env.Check(c.root(), t)
	if st.Running {
		c.reply(msg.ID, map[string]any{"status": st}, nil)
		return
	}
	consent := "asked"
	if p.Always {
		if pr := a.May(ollama.OpStart, t, st); pr == nil {
			if err := a.Env.SetStartAlways(true); err != nil {
				warnf("the choice to always start Ollama could not be kept: %v", err)
			}
		}
	} else if a.Env.StartAlways() {
		consent = "always"
	}
	s, pr := a.Start(c.root(), t, st, consent)
	if pr != nil {
		c.reply(msg.ID, nil, ollamaRefusal(pr, a.Env.Check(c.root(), t)))
		return
	}
	c.reply(msg.ID, map[string]any{"started": s, "status": a.Env.Check(c.root(), t)}, nil)
}

func (c *acpConn) ollamaInstall(msg rpcMessage) {
	_, _, a, t, e := c.ollamaTarget(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	st := a.Env.Check(c.root(), t)
	if st.Installed || st.Running {
		c.reply(msg.ID, map[string]any{"status": st}, nil)
		return
	}
	plan := a.Env.PlanInstall()
	var buf tailBuffer
	// Studio has no terminal for a password prompt, so the script is never run here.
	if pr := a.Install(c.root(), t, st, plan, false, nil, &buf, &buf); pr != nil {
		c.reply(msg.ID, nil, ollamaRefusal(pr, a.Env.Check(c.root(), t)))
		return
	}
	outcome := agent.OllamaOK
	if plan.Kind == ollama.InstallOpen {
		outcome = agent.OllamaManual
	}
	c.reply(msg.ID, map[string]any{"outcome": outcome, "output": buf.String(), "status": a.Env.Check(c.root(), t)}, nil)
}

// pulls are the downloads running, by pullId, for _abhed/ollama/cancel.
var (
	pullsMu sync.Mutex
	pulls   = map[string]context.CancelFunc{}
)

func (c *acpConn) ollamaPull(msg rpcMessage) {
	s, p, a, t, e := c.ollamaTarget(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if p.PullID == "" {
		c.reply(msg.ID, nil, refusal(errParams, "a pull needs a pullId, for its progress and its cancel"))
		return
	}
	ctx, cancel := context.WithCancel(c.root())
	defer cancel()
	pullsMu.Lock()
	if _, dup := pulls[p.PullID]; dup {
		pullsMu.Unlock()
		c.reply(msg.ID, nil, refusal(errParams, "a pull with that pullId is running"))
		return
	}
	pulls[p.PullID] = cancel
	pullsMu.Unlock()
	defer func() {
		pullsMu.Lock()
		delete(pulls, p.PullID)
		pullsMu.Unlock()
	}()
	st := a.Env.Check(ctx, t)
	if st.Pulled {
		c.reply(msg.ID, map[string]any{"status": st}, nil)
		return
	}
	if pr := a.May(ollama.OpPull, t, st); pr != nil {
		a.Note(ollama.OpPull, agent.OllamaRefused, t, pr)
		c.reply(msg.ID, nil, ollamaRefusal(pr, st))
		return
	}
	fit, pr := a.Env.CheckFit(ctx, t, st)
	if pr != nil {
		a.Note(ollama.OpPull, agent.OllamaFailed, t, pr)
		c.reply(msg.ID, nil, ollamaRefusal(pr, st))
		return
	}
	pr = a.Pull(ctx, t, st, fit, func(pg ollama.Progress) {
		c.notification("_abhed/ollama/progress", map[string]any{"sessionId": s.id, "pullId": p.PullID,
			"status": pg.Status, "completed": pg.Completed, "total": pg.Total})
	})
	if pr != nil {
		c.reply(msg.ID, nil, ollamaRefusal(pr, a.Env.Check(c.root(), t)))
		return
	}
	c.reply(msg.ID, map[string]any{"status": a.Env.Check(c.root(), t)}, nil)
}

func (c *acpConn) ollamaCancel(msg rpcMessage) {
	var p ollamaParams
	_ = json.Unmarshal(msg.Params, &p)
	pullsMu.Lock()
	cancel, ok := pulls[p.PullID]
	pullsMu.Unlock()
	if ok {
		cancel()
	}
	c.reply(msg.ID, map[string]any{"cancelled": ok}, nil)
}

func (c *acpConn) ollamaForget(msg rpcMessage) {
	if err := ollamaEnv().SetStartAlways(false); err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the choice could not be forgotten: %v", err))
		return
	}
	c.reply(msg.ID, map[string]any{}, nil)
}

// ollamaMeta is the chosen model's Ollama state for a model switch's
// reply, when a local Ollama serves it and it is not ready; nil otherwise.
// It only reads this machine: no registry, no download host.
func (c *acpConn) ollamaMeta(s *acpSession, name string) any {
	if s == nil || !s.inner {
		return nil
	}
	t, ok := toolset.OllamaTarget(s.parts.Config, name)
	if !ok {
		return nil
	}
	st := ollamaEnv().Check(c.root(), t)
	if st.Ready {
		return nil
	}
	return st
}
