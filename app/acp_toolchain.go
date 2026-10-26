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
	liveFeatures = append(liveFeatures, "toolchain")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/toolchain/status":  (*acpConn).toolchainStatus,
		"_abhed/toolchain/install": (*acpConn).toolchainInstall,
		"_abhed/toolchain/cancel":  (*acpConn).toolchainCancel,
	})
}

// toolchainParams are what every _abhed/toolchain method takes.
type toolchainParams struct {
	SessionID string `json:"sessionId"`
	// Language is a language (go, python, node, rust, java, maven, gradle,
	// ruby, dotnet) or a program one provides (cargo, python3, mvn…), as a
	// command that exited 127 named it.
	Language string `json:"language"`
	// Version is the person's own version; "" is the project's pin, else
	// the latest stable.
	Version string `json:"version"`
	// InstallID names an install, for its progress and cancel.
	InstallID string `json:"installId"`
	// Debugger asks for the language's debugger too (Delve for Go, debugpy
	// for Python), offered and agreed to with it.
	Debugger bool `json:"debugger"`
	// Agreed is what the person agreed to from status's offer; an install
	// that would now fetch something else is refused.
	Agreed *toolchainAgreed `json:"agreed"`
}

// toolchainAgreed is the offer's version and digests the modal showed.
type toolchainAgreed struct {
	Version         string `json:"version"`
	Digest          string `json:"digest"`
	DebuggerVersion string `json:"debugger_version"`
	DebuggerDigest  string `json:"debugger_digest"`
	// Registries are the registry hosts the modal showed; only an install
	// that lists them, as status offers them, opens them.
	Registries []string `json:"registries"`
}

// toolchainProblem is an error's data, as the Ollama assist's is.
type toolchainProblem struct {
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	Steps   []string `json:"steps,omitempty"`
}

func toolchainError(code int, kind, message string) *rpcError {
	return &rpcError{Code: code, Message: message, Data: map[string]any{
		"problem": toolchainProblem{Kind: kind, Message: message, Steps: problemSteps[kind]}}}
}

// problemSteps are what a person can do about a problem.
var problemSteps = map[string][]string{
	"toolchains_off": {"Set sandbox.toolchains to ask in your own configuration, where the managed file allows it"},
	"no_build":       {"Name another version, or set sandbox.image to an image that has the toolchain"},
	"changed":        {"Ask for the offer again (_abhed/toolchain/status) and show it before installing"},
	"no_debugger": {"Debug in a Linux sandbox (the container tier, or the process tier on Linux)",
		"For Go, use Go 1.25 or later; for Python, 3.10 or later", "Or use a sandbox image that has the debugger"},
}

// registriesOffer is what an install of lang would open to commands, or why
// it would open none.
func registriesOffer(tc *toolchain.Session, lang string) ([]string, string) {
	if tc.Registries == nil || tc.Open == nil {
		return []string{}, "this session opens no package registries"
	}
	hosts, why := tc.Registries(lang)
	if why != "" {
		return []string{}, why
	}
	if hosts == nil {
		hosts = []string{}
	}
	return hosts, ""
}

// sameHosts reports whether two host lists name the same hosts.
func sameHosts(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, h := range a {
		seen[h] = true
	}
	for _, h := range b {
		if !seen[h] {
			return false
		}
	}
	return true
}

// debuggerPlan is the plan for the debugger of plan's language.
func debuggerPlan(ctx context.Context, tc *toolchain.Session, plan toolchain.Plan) (toolchain.ExtraPlan, error) {
	e, ok := toolchain.DebuggerFor(plan.Spec.Lang)
	if !ok {
		return toolchain.ExtraPlan{}, errors.New("no debugger is installed with " + plan.Spec.Lang + "; Abhed installs one with Go (Delve) and Python (debugpy)")
	}
	return tc.PlanExtra(ctx, e, plan)
}

// toolchainLang is the language a name or a program means.
func toolchainLang(name string) (toolchain.Lang, bool) {
	if l, ok := toolchain.Find(name); ok {
		return l, true
	}
	return toolchain.ForProgram(name)
}

// toolchainSession finds the session's toolchains and the request's spec.
func (c *acpConn) toolchainSession(msg rpcMessage) (*acpSession, toolchainParams, *toolchain.Session, toolchain.Spec, *rpcError) {
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
		return nil, p, nil, toolchain.Spec{}, toolchainError(errPolicy, "unavailable", "toolchains are not available in this session")
	}
	tc := *parts.Set.Toolchains
	if parts.Session != nil {
		tc.Workspace, tc.Root = parts.Session.WorkDir(), parts.Session.Root
	}
	tc.Record = func(a agent.ToolchainAction) { s.record(agent.EvToolchainInstall, a) }
	l, ok := toolchainLang(p.Language)
	if !ok {
		return nil, p, nil, toolchain.Spec{}, toolchainError(errParams, "unknown_language",
			"Abhed does not install a toolchain for "+p.Language+"; it installs "+toolchain.Names())
	}
	v, err := toolchain.NormalizeVersion(l.Name, p.Version)
	if err != nil {
		return nil, p, nil, toolchain.Spec{}, toolchainError(errParams, "bad_version", err.Error())
	}
	return s, p, &tc, toolchain.Spec{Lang: l.Name, Version: v}, nil
}

// toolchainOffer is what an install would do, for Studio's modal.
type toolchainOffer struct {
	Command string `json:"command"`
	Source  string `json:"source"`
	Size    int64  `json:"size,omitempty"`
	Version string `json:"version,omitempty"`
	Into    string `json:"into,omitempty"`
	// Why the version: requested, pinned by a file, or latest stable.
	Why string `json:"why,omitempty"`
	// URL and Digest are the download and the checksum it is checked against.
	URL    string `json:"url,omitempty"`
	Digest string `json:"digest,omitempty"`
	// Title names everything the offer installs, for the modal's heading.
	Title string `json:"title"`
	// Registries are the package registry hosts the install opens to the
	// session's commands through the egress proxy; RegistriesNote says why
	// none are opened, when that is so. The modal shows both.
	Registries     []string `json:"registries"`
	RegistriesNote string   `json:"registries_note,omitempty"`
	// Debugger is the debugger installed with it, when asked for.
	Debugger *debuggerOffer `json:"debugger,omitempty"`
}

// debuggerOffer is the debugger part of an offer.
type debuggerOffer struct {
	Name      string `json:"name"`
	Display   string `json:"display"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	URL       string `json:"url,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Note      string `json:"note,omitempty"`
	Installed bool   `json:"installed"`
}

// toolchainStatus says whether the session's commands have the language,
// and what an install would fetch. It installs nothing and is not recorded.
func (c *acpConn) toolchainStatus(msg rpcMessage) {
	_, p, tc, spec, e := c.toolchainSession(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	out := map[string]any{"language": spec.Lang, "available": false, "can_act": !tc.Off && inAgentCommand() == ""}
	for _, in := range tc.Active(tc.Workspace) {
		if in.Lang == spec.Lang {
			out["available"], out["version"] = true, in.Version
		}
	}
	if e, ok := toolchain.DebuggerFor(spec.Lang); ok {
		_, has := tc.Cache.LookupExtra(tc.Platform(), e)
		out["debugger"] = map[string]any{"name": e.Name, "version": e.Version, "available": has && out["available"] == true}
	}
	switch {
	case tc.Off:
		out["refusal"] = toolchainProblem{Kind: "toolchains_off", Message: toolchain.ErrOff.Error(),
			Steps: []string{"Set sandbox.toolchains to ask in your own configuration, where the managed file allows it"}}
	case inAgentCommand() != "":
		out["refusal"] = toolchainProblem{Kind: "in_agent", Message: "installing a toolchain is refused inside an agent's command"}
	default:
		plan, err := tc.Plan(c.root(), spec)
		if err != nil {
			out["can_act"] = false
			out["refusal"] = toolchainProblem{Kind: "no_build", Message: err.Error()}
			break
		}
		l, _ := toolchain.Find(plan.Spec.Lang)
		o := toolchainOffer{Command: "abhed toolchain install " + plan.Spec.Lang + "@" + plan.Version(), Version: plan.Version(), Why: plan.Why,
			Source: plan.Artifact.Source, Size: plan.Artifact.Size, URL: plan.Artifact.URL, Digest: plan.Artifact.Digest,
			Into: tc.Cache.Dir(tc.Platform(), plan.Spec.Lang, plan.Version()), Title: l.Display + " " + plan.Version()}
		if plan.Installed != nil {
			o.Source, o.URL, o.Digest = "installed already", plan.Installed.URL, plan.Installed.Digest
		}
		o.Registries, o.RegistriesNote = registriesOffer(tc, plan.Spec.Lang)
		if p.Debugger {
			ep, err := debuggerPlan(c.root(), tc, plan)
			if err != nil {
				out["can_act"] = false
				out["refusal"] = toolchainProblem{Kind: "no_debugger", Message: err.Error(), Steps: problemSteps["no_debugger"]}
				break
			}
			o.Command = "abhed toolchain install -debugger " + plan.Spec.Lang + "@" + plan.Version()
			o.Title += " + " + ep.Extra.Display + " " + ep.Extra.Version
			o.Debugger = &debuggerOffer{Name: ep.Extra.Name, Display: ep.Extra.Display, Version: ep.Extra.Version, Source: ep.Source,
				URL: ep.URL, Digest: ep.Digest, Size: ep.Size, Note: ep.Note, Installed: ep.Installed != nil}
		}
		out["offer"] = o
	}
	c.reply(msg.ID, out, nil)
}

// toolchainInstalls are the installs running, by installId, for cancel.
var (
	toolchainInstallsMu sync.Mutex
	toolchainInstalls   = map[string]context.CancelFunc{}
)

// toolchainInstall installs after the person agreed in Studio.
func (c *acpConn) toolchainInstall(msg rpcMessage) {
	s, p, tc, spec, e := c.toolchainSession(msg)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, toolchainError(errPolicy, "in_agent", "installing a toolchain is refused inside an agent's command ("+why+")"))
		return
	}
	if p.InstallID == "" {
		c.reply(msg.ID, nil, toolchainError(errParams, "no_install_id", "an install needs an installId, for its progress and its cancel"))
		return
	}
	ctx, cancel := context.WithCancel(c.root())
	defer cancel()
	toolchainInstallsMu.Lock()
	if _, dup := toolchainInstalls[p.InstallID]; dup {
		toolchainInstallsMu.Unlock()
		c.reply(msg.ID, nil, toolchainError(errParams, "duplicate", "an install with that installId is running"))
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
		if errors.Is(err, toolchain.ErrOff) {
			tc.Record(agent.ToolchainAction{Lang: spec.Lang, Version: spec.Version, Outcome: agent.ToolchainRefused, Error: err.Error(), By: agent.ByUser, Surface: "acp"})
			c.reply(msg.ID, nil, toolchainError(errPolicy, "toolchains_off", err.Error()))
			return
		}
		c.reply(msg.ID, nil, toolchainError(errRefused, "no_build", err.Error()))
		return
	}
	// The debugger is planned before anything is installed, so a debugger
	// that cannot be had there installs nothing.
	var ep *toolchain.ExtraPlan
	if p.Debugger {
		d, err := debuggerPlan(ctx, tc, plan)
		if err != nil {
			c.reply(msg.ID, nil, toolchainError(errRefused, "no_debugger", err.Error()))
			return
		}
		ep = &d
	}
	if a := p.Agreed; a != nil {
		digest := plan.Artifact.Digest
		if plan.Installed != nil {
			digest = plan.Installed.Digest
		}
		changed := a.Version != plan.Version() || a.Digest != "" && a.Digest != digest
		if ep != nil {
			changed = changed || a.DebuggerVersion != ep.Extra.Version || a.DebuggerDigest != "" && a.DebuggerDigest != ep.Digest
		}
		if hosts, _ := registriesOffer(tc, plan.Spec.Lang); !sameHosts(hosts, a.Registries) {
			changed = true
		}
		if changed {
			c.reply(msg.ID, nil, toolchainError(errRefused, "changed",
				"what would be installed has changed since it was offered, so nothing was installed; ask for the offer again"))
			return
		}
	}
	// An install that does not say what the person saw is still made, for an
	// older Studio, but recorded as unconfirmed and opens no registries.
	consent := "studio"
	if p.Agreed == nil {
		consent = "studio-unconfirmed"
	}
	progress := func(status string) toolchain.Progress {
		return func(done, total int64) {
			c.notification("_abhed/toolchain/progress", map[string]any{"sessionId": s.id, "installId": p.InstallID,
				"status": status, "completed": done, "total": total})
		}
	}
	failed := func(err error) {
		kind := "install_failed"
		if ctx.Err() != nil {
			kind = "cancelled"
		}
		c.reply(msg.ID, nil, toolchainError(errRefused, kind, err.Error()))
	}
	in, err := tc.Install(ctx, plan, agent.ByUser, "acp", consent, progress("downloading"))
	if err != nil {
		failed(err)
		return
	}
	res := map[string]any{"language": in.Lang, "available": true, "version": in.Version, "dir": in.Dir, "bin": toolchain.BinDirs(in)}
	if ep != nil {
		x, err := tc.InstallExtra(ctx, *ep, in, agent.ByUser, "acp", consent, progress("debugger"))
		if err != nil {
			failed(err)
			return
		}
		bin, py := toolchain.ExtraEnv(x)
		res["debugger"] = map[string]any{"name": x.Name, "version": x.Version, "dir": x.Dir, "bin": bin, "pythonpath": py}
	}
	if p.Agreed != nil {
		res["registries"] = tc.OpenRegistriesFor(in.Lang, "acp", func(r agent.ToolchainRegistry) { s.record(agent.EvToolchainRegistry, r) })
	} else {
		res["registries"] = "Its package registries were not opened: the install did not confirm them (agreed.registries)."
	}
	c.reply(msg.ID, res, nil)
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
