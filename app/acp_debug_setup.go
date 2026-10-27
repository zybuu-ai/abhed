package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/debugrelay"
	"github.com/zybuu-ai/abhed/internal/enginecli"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// When a debug session needs the container tier and it is not set up, the
// engine says what is missing and the exact steps (container-setup), which
// Studio can walk the person through; or, on the person's explicit
// agreement for the workspace, debugs on the process tier with loopback
// open to the debug command alone.

func init() {
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/debug/status-tier":    (*acpConn).debugStatusTier,
		"_abhed/debug/use-container":  (*acpConn).debugUseContainer,
		"_abhed/debug/forget-consent": (*acpConn).debugForgetConsent,
	})
}

// debugSetup is what the container tier needs here and what is missing.
type debugSetup struct {
	OS string `json:"os"`
	// Tier is the tier the session's commands run under.
	Tier string `json:"tier"`
	// Ready is set when a debug session can start as things are.
	Ready bool `json:"ready"`
	// Engine is the podman (or docker) program found, "" when none is.
	Engine string `json:"engine,omitempty"`
	// Brew is Homebrew, for installing podman, "" when none is.
	Brew string `json:"brew,omitempty"`
	// Machine is a Mac's podman machine: none, stopped or running; "" where
	// no machine is needed or no podman is found.
	Machine string `json:"machine,omitempty"`
	// Steps are what to do, in order, each with the exact command where
	// there is one; done steps are left out.
	Steps []debugSetupStep `json:"steps"`
	// ProcessTier says whether this workspace may instead debug on the
	// process tier, and whether the person already agreed for it.
	ProcessTier debugProcessTier `json:"processTier"`
}

type debugSetupStep struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Command string `json:"command,omitempty"`
}

type debugProcessTier struct {
	Available bool   `json:"available"`
	Consented bool   `json:"consented"`
	Consent   string `json:"consent,omitempty"`
}

// debugLoopbackWhy is what the person agrees to for debugging on the
// process tier here: put to them until they allow it for the workspace,
// recorded.
var debugLoopbackWhy = debugLoopbackWhyOn(runtime.GOOS)

// debugLoopbackWhyOn is that agreement's text on goos: on macOS loopback is
// opened to the debug command alone; on Linux the session's network is
// open already, so the program reaches all of it.
func debugLoopbackWhyOn(goos string) string {
	keep := " Allow for this workspace keeps the choice; Allow once is for this debug session only"
	if goos == "linux" {
		return "debug on this machine's process sandbox, whose network is open, instead of a container: while it is being debugged, the program " +
			"can reach services on this machine (localhost ports) and the network, as this session's commands can, and local services can reach it." + keep
	}
	return "debug on this machine's process sandbox instead of a container: while it is being debugged, the program can reach " +
		"services on this Mac (localhost ports) and they can reach it; nothing else on the network." + keep
}

// debugEngineProbe finds the container engine, Homebrew and a Mac's podman
// machine; tests replace it.
var debugEngineProbe = func(ctx context.Context) (engine, brew, machine string) {
	engine = findProgram("podman", "/opt/homebrew/bin/podman", "/usr/local/bin/podman", "/opt/podman/bin/podman")
	if engine == "" {
		engine = findProgram("docker", "/usr/local/bin/docker", "/opt/homebrew/bin/docker")
	}
	brew = findProgram("brew", "/opt/homebrew/bin/brew", "/usr/local/bin/brew")
	if engine == "" || runtime.GOOS != "darwin" || filepath.Base(engine) != "podman" {
		return engine, brew, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := enginecli.Command(ctx, engine, "machine", "list", "--format", "json").Output()
	if err != nil {
		return engine, brew, ""
	}
	var ms []struct {
		Running  bool
		Starting bool
	}
	if json.Unmarshal(out, &ms) != nil {
		return engine, brew, ""
	}
	machine = "none"
	for _, m := range ms {
		switch {
		case m.Running:
			return engine, brew, "running"
		default:
			machine = "stopped"
		}
	}
	return engine, brew, machine
}

// findProgram is name on PATH, else the first of places that is a program.
func findProgram(name string, places ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, p := range places {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// debugProcessTierPossible reports whether a refused tier may still debug on
// the person's agreement: the macOS process tier (loopback is opened to the
// debug command alone), and the Linux process tier with the network open
// (whose loopback is the host's already).
//
// Not under the egress allowlist: loopback would also reach the egress
// proxies, its own calls' and other sessions', which hold allowlists this
// debug session was never given.
func debugProcessTierPossible(tier string, iso tools.Isolation) bool {
	return debugProcessTierPossibleOn(runtime.GOOS, tier, iso)
}

func debugProcessTierPossibleOn(goos, tier string, iso tools.Isolation) bool {
	if tier != string(sandbox.TierProcess) || iso.Allowlist {
		return false
	}
	return goos == "darwin" || goos == "linux" && iso.Network
}

// debugSetupFor is what the session at workspace needs to debug.
func debugSetupFor(ctx context.Context, tier string, iso tools.Isolation, workspace string) debugSetup {
	st := debugSetup{OS: runtime.GOOS, Tier: tier, Ready: debugTierProblem(tier, iso) == nil}
	st.ProcessTier = debugProcessTier{Available: debugProcessTierPossible(tier, iso)}
	if st.ProcessTier.Available {
		st.ProcessTier.Consented = debugConsented(workspace)
		st.ProcessTier.Consent = debugLoopbackWhy
	}
	if st.Ready {
		return st
	}
	st.Engine, st.Brew, st.Machine = debugEngineProbe(ctx)
	if st.Engine == "" {
		switch {
		case st.Brew != "":
			st.Steps = append(st.Steps, debugSetupStep{ID: "install-podman", Title: "Install Podman with Homebrew", Command: "brew install podman"})
		case runtime.GOOS == "linux":
			st.Steps = append(st.Steps, debugSetupStep{ID: "install-podman", Title: "Install Podman with your distribution's package manager (for example: sudo apt install podman, or sudo dnf install podman)"})
		default:
			st.Steps = append(st.Steps, debugSetupStep{ID: "install-podman", Title: "Install Podman from podman.io (or Docker Desktop)"})
		}
	}
	if runtime.GOOS == "darwin" && (st.Engine == "" || filepath.Base(st.Engine) == "podman") {
		podman := "podman"
		if st.Engine != "" {
			podman = st.Engine
		}
		switch st.Machine {
		case "", "none":
			st.Steps = append(st.Steps, debugSetupStep{ID: "machine-init", Title: "Create Podman's virtual machine", Command: podman + " machine init"})
			st.Steps = append(st.Steps, debugSetupStep{ID: "machine-start", Title: "Start it", Command: podman + " machine start"})
		case "stopped":
			st.Steps = append(st.Steps, debugSetupStep{ID: "machine-start", Title: "Start Podman's virtual machine", Command: podman + " machine start"})
		}
	}
	if tier != string(sandbox.TierContainer) && tier != string(sandbox.TierVM) {
		st.Steps = append(st.Steps, debugSetupStep{ID: "use-container-tier",
			Title: "Run this workspace's commands in a container: {\"sandbox\": {\"min_tier\": \"container\"}} in " + filepath.Join(workspace, ".abhed", "config.json") +
				" (_abhed/debug/use-container writes it), then open a new session"})
	}
	return st
}

// debugSetupProblem is the container-setup problem for a refused tier: the
// tier's own message, the steps as commands, and the setup for Studio.
func debugSetupProblem(p *debugrelay.Problem, st debugSetup) *rpcError {
	steps := make([]string, 0, len(st.Steps)+1)
	for _, s := range st.Steps {
		if s.Command != "" {
			steps = append(steps, s.Title+": "+s.Command)
		} else {
			steps = append(steps, s.Title)
		}
	}
	if st.ProcessTier.Available {
		steps = append(steps, "Or debug on this machine's process sandbox, after agreeing that "+debugLoopbackWhy+": launch again with processTier: true")
	}
	prob := &debugrelay.Problem{Kind: debugrelay.KindContainerSetup, Message: p.Message, Steps: steps, Retry: true}
	return &rpcError{Code: errRefused, Message: p.Message, Data: map[string]any{"problem": prob, "setup": st}}
}

func (c *acpConn) debugStatusTier(msg rpcMessage) {
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
	tool, _ := parts.Loop.Tools.Get("bash")
	b, ok := tool.(tools.Bash)
	tier, iso := "none", tools.Isolation{}
	if ok && b.Sandbox != nil {
		tier, iso = debugTier(b), b.Isolation
	}
	c.reply(msg.ID, debugSetupFor(c.root(), tier, iso, parts.Session.Root), nil)
}

// debugUseContainer raises the workspace's tier to the container, at the
// person's request from Studio's setup: sandbox.min_tier in the workspace's
// own configuration, which a workspace file may always raise. It takes
// effect in a new session.
func (c *acpConn) debugUseContainer(msg rpcMessage) {
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
	path, raised, err := raiseWorkspaceTier(parts.Session.Root, sandbox.TierContainer)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the workspace's configuration could not be written: %v", err))
		return
	}
	if !raised {
		// Already at least the container tier: never lowered.
		c.reply(msg.ID, map[string]any{"path": path, "newSession": true, "already": true}, nil)
		return
	}
	s.record(agent.EvDebugSetup, agent.DebugSetup{Action: "use_container", Workspace: parts.Session.Root, Path: path})
	c.reply(msg.ID, map[string]any{"path": path, "newSession": true}, nil)
}

// raiseWorkspaceTier sets sandbox.min_tier in workspace/.abhed/config.json
// to tier when it is absent or weaker, keeping the rest of the file; a
// stronger one is left as it is (raised false).
func raiseWorkspaceTier(workspace string, tier sandbox.Tier) (string, bool, error) {
	dir := filepath.Join(workspace, ".abhed")
	path := filepath.Join(dir, "config.json")
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New(dir + " is a link")
	}
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			return "", false, errors.New(path + " is not a plain file")
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return "", false, errors.New(path + " is not JSON: " + err.Error())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	sb, _ := cfg["sandbox"].(map[string]any)
	if sb == nil {
		sb = map[string]any{}
	}
	if cur, _ := sb["min_tier"].(string); cur != "" && sandbox.Tier(cur).Strength() >= tier.Strength() {
		return path, false, nil
	}
	sb["min_tier"] = string(tier)
	cfg["sandbox"] = sb
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return "", false, err
	}
	return path, true, os.Rename(tmp, path)
}

func (c *acpConn) debugForgetConsent(msg rpcMessage) {
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
	if err := setDebugConsent(parts.Session.Root, false); err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "the choice could not be forgotten: %v", err))
		return
	}
	s.record(agent.EvDebugSetup, agent.DebugSetup{Action: "process_tier_forgotten", Workspace: parts.Session.Root})
	c.reply(msg.ID, map[string]any{}, nil)
}

// debugConsentFile keeps the workspaces whose person agreed to debug on the
// process tier.
var debugConsentFile = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".abhed", "state", "debug-process-tier.json")
}

var debugConsentMu sync.Mutex

type debugConsents struct {
	Workspaces map[string]time.Time `json:"workspaces"`
}

func debugWorkspaceKey(ws string) string {
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		return r
	}
	return filepath.Clean(ws)
}

// readDebugConsents reads the agreements; bad is set when the file is
// there but cannot be read as one, which then counts as no agreement.
func readDebugConsents() (c debugConsents, bad bool) {
	if f := debugConsentFile(); f != "" {
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &c) != nil {
			warnf("%s is not a list of agreements to debug on the process tier, so none is taken from it", f)
			bad = true
			c = debugConsents{}
		}
	}
	if c.Workspaces == nil {
		c.Workspaces = map[string]time.Time{}
	}
	return c, bad
}

// debugConsented reports whether the person agreed to debug workspace on the
// process tier.
func debugConsented(workspace string) bool {
	debugConsentMu.Lock()
	defer debugConsentMu.Unlock()
	c, _ := readDebugConsents()
	_, ok := c.Workspaces[debugWorkspaceKey(workspace)]
	return ok
}

// setDebugConsent keeps, or with on false forgets, the agreement for workspace.
func setDebugConsent(workspace string, on bool) error {
	debugConsentMu.Lock()
	defer debugConsentMu.Unlock()
	f := debugConsentFile()
	if f == "" {
		return errors.New("there is no home folder to keep the choice in")
	}
	c, bad := readDebugConsents()
	if bad {
		// Kept aside, not overwritten: what it held is the person's to look at.
		aside := f + ".unreadable-" + time.Now().UTC().Format("20060102T150405")
		if err := os.Rename(f, aside); err != nil {
			return err
		}
		warnf("the unreadable %s was moved to %s", f, aside)
	}
	if on {
		c.Workspaces[debugWorkspaceKey(workspace)] = time.Now().UTC()
	} else {
		delete(c.Workspaces, debugWorkspaceKey(workspace))
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}
