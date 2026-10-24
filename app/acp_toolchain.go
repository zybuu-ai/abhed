package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolchain"
)

// Language toolchains for a Studio session's commands
// (studio-acp-contract.md §3.2b).
//
// Studio shows the offer (source, size, checksum) and asks the person in its
// own modal before it sends install; the engine still applies
// sandbox.toolchains, refuses inside an agent's command, installs only from
// the official sources, verified, and records every attempt.

func init() {
	liveFeatures = append(liveFeatures, "toolchains")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/toolchain/status":  (*acpConn).toolchainStatus,
		"_abhed/toolchain/offer":   (*acpConn).toolchainOffer,
		"_abhed/toolchain/install": (*acpConn).toolchainInstall,
		"_abhed/toolchain/cancel":  (*acpConn).toolchainCancel,
	})
}

// toolchainParams are what every _abhed/toolchain method takes.
type toolchainParams struct {
	SessionID string `json:"sessionId"`
	// Lang is go, python, node, rust, java, maven, gradle, ruby or dotnet.
	Lang string `json:"lang"`
	// Version is the person's own version, or "" for the project's pin or
	// the latest stable.
	Version string `json:"version"`
	// InstallID names an install, for its progress and cancel.
	InstallID string `json:"installId"`
}

// toolchainSession finds the session's toolchains and the request's spec.
func (c *acpConn) toolchainSession(msg rpcMessage, needLang bool) (*acpSession, toolchainParams, *toolchain.Session, toolchain.Spec, *rpcError) {
	var p toolchainParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return nil, p, nil, toolchain.Spec{}, badParams("params", err)
	}
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		return nil, p, nil, toolchain.Spec{}, e
	}
	parts, e := innerOf(s)
	if e != nil {
		return nil, p, nil, toolchain.Spec{}, e
	}
	if parts.Set == nil || parts.Set.Toolchains == nil {
		return nil, p, nil, toolchain.Spec{}, refusal(errPolicy, "toolchains are not available in this session")
	}
	tc := *parts.Set.Toolchains
	if parts.Session != nil {
		tc.Workspace = parts.Session.WorkDir()
	}
	tc.Record = func(a agent.ToolchainAction) { s.record(agent.EvToolchainInstall, a) }
	var spec toolchain.Spec
	if needLang {
		var err error
		if spec, err = toolchain.ParseSpec(p.Lang); err != nil {
			return nil, p, nil, spec, refusal(errParams, "%s", err.Error())
		}
		if spec.Version, err = toolchain.NormalizeVersion(spec.Lang, p.Version); err != nil {
			return nil, p, nil, spec, refusal(errParams, "%s", err.Error())
		}
	}
	return s, p, &tc, spec, nil
}

// toolchainStatus is what the session's commands have, what the workspace
// pins, and whether installs are on.
func (c *acpConn) toolchainStatus(msg rpcMessage) {
	_, _, tc, _, e := c.toolchainSession(msg, false)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	p := tc.Platform()
	c.reply(msg.ID, map[string]any{"enabled": !tc.Off, "platform": p.String(), "active": tc.Active(tc.Workspace),
		"installed": tc.Cache.List(p, ""), "pins": toolchain.PinsFrom(tc.Workspace)}, nil)
}

// toolchainOffer is what an install would fetch: the version and why, the
// source, the download, its size and published checksum. It installs nothing.
func (c *acpConn) toolchainOffer(msg rpcMessage) {
	_, _, tc, spec, e := c.toolchainSession(msg, true)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	plan, err := tc.Plan(c.root(), spec)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "%s", err.Error()))
		return
	}
	c.reply(msg.ID, map[string]any{"plan": plan, "version": plan.Version()}, nil)
}

// toolchainInstalls are the installs running, by installId, for cancel.
var (
	toolchainInstallsMu sync.Mutex
	toolchainInstalls   = map[string]context.CancelFunc{}
)

// toolchainInstall installs after the person agreed in Studio.
func (c *acpConn) toolchainInstall(msg rpcMessage) {
	s, p, tc, spec, e := c.toolchainSession(msg, true)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, refusal(errPolicy, "installing a toolchain is refused inside an agent's command (%s)", why))
		return
	}
	if p.InstallID == "" {
		c.reply(msg.ID, nil, refusal(errParams, "an install needs an installId, for its progress and its cancel"))
		return
	}
	ctx, cancel := context.WithCancel(c.root())
	defer cancel()
	toolchainInstallsMu.Lock()
	if _, dup := toolchainInstalls[p.InstallID]; dup {
		toolchainInstallsMu.Unlock()
		c.reply(msg.ID, nil, refusal(errParams, "an install with that installId is running"))
		return
	}
	toolchainInstalls[p.InstallID] = cancel
	toolchainInstallsMu.Unlock()
	defer func() {
		toolchainInstallsMu.Lock()
		delete(toolchainInstalls, p.InstallID)
		toolchainInstallsMu.Unlock()
	}()
	plan, err := tc.Plan(ctx, spec)
	if err != nil {
		code := errRefused
		if errors.Is(err, toolchain.ErrOff) {
			code = errPolicy
			tc.Record(agent.ToolchainAction{Lang: spec.Lang, Version: spec.Version, Outcome: agent.ToolchainRefused, Error: err.Error(), By: agent.ByUser, Surface: "acp"})
		}
		c.reply(msg.ID, nil, refusal(code, "%s", err.Error()))
		return
	}
	in, err := tc.Install(ctx, plan, agent.ByUser, "acp", "studio", func(done, total int64) {
		c.notification("_abhed/toolchain/progress", map[string]any{"sessionId": s.id, "installId": p.InstallID,
			"completed": done, "total": total})
	})
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "%s", err.Error()))
		return
	}
	c.reply(msg.ID, map[string]any{"installed": in, "dir": in.Dir, "bin": toolchain.BinDirs(in)}, nil)
}

func (c *acpConn) toolchainCancel(msg rpcMessage) {
	var p toolchainParams
	_ = json.Unmarshal(msg.Params, &p)
	toolchainInstallsMu.Lock()
	cancel, ok := toolchainInstalls[p.InstallID]
	toolchainInstallsMu.Unlock()
	if ok {
		cancel()
	}
	c.reply(msg.ID, map[string]any{"cancelled": ok}, nil)
}
