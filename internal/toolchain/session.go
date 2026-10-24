package toolchain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// Session is the toolchains of one session's commands: the cache, the
// sources, where its commands run, and the record each install goes to.
type Session struct {
	Cache     Cache
	Source    *Source
	Workspace string
	// Platform is where commands run now: a container sees Linux builds.
	Platform func() Platform
	// Off is sandbox.toolchains off: nothing is installed, and the cache is
	// not offered to commands.
	Off bool
	// Record writes a toolchain.install event; nil records nothing.
	Record func(agent.ToolchainAction)
}

func (s *Session) platform() Platform {
	if s.Platform == nil {
		return Host()
	}
	return s.Platform()
}

// Plan is what an install would do: the version chosen and why, and either
// the build to download or the one installed already.
type Plan struct {
	Spec      Spec       `json:"spec"`
	Why       string     `json:"why"`
	Artifact  Artifact   `json:"artifact"`
	Installed *Installed `json:"installed,omitempty"`
}

// Version is the version the plan installs.
func (p Plan) Version() string {
	if p.Installed != nil {
		return p.Installed.Version
	}
	return p.Artifact.Version
}

// Plan chooses the version for a request (explicit, else the project's
// pin, else the latest stable) and finds it in the cache or the source.
func (s *Session) Plan(ctx context.Context, requested Spec) (Plan, error) {
	if s.Off {
		return Plan{}, ErrOff
	}
	spec, why := Choose(requested, PinsFrom(s.Workspace))
	p := s.platform()
	if in, ok := s.Cache.Best(p, spec); ok && spec.Version != "" {
		return Plan{Spec: spec, Why: why, Installed: &in}, nil
	}
	a, err := s.Source.Resolve(ctx, spec, p)
	if err != nil {
		return Plan{}, err
	}
	if in, ok := s.Cache.Lookup(p, spec.Lang, a.Version); ok {
		return Plan{Spec: spec, Why: why, Artifact: a, Installed: &in}, nil
	}
	return Plan{Spec: spec, Why: why, Artifact: a}, nil
}

// ErrOff is any install where sandbox.toolchains is off.
var ErrOff = fmt.Errorf("installing toolchains is turned off here (sandbox.toolchains off)")

// Install carries out a plan, recording how it ended.
func (s *Session) Install(ctx context.Context, plan Plan, by, surface, consent string, progress Progress) (Installed, error) {
	ev := agent.ToolchainAction{Lang: plan.Spec.Lang, Version: plan.Version(), Platform: s.platform().String(), Why: plan.Why,
		Source: plan.Artifact.Source, URL: plan.Artifact.URL, Digest: plan.Artifact.Digest, Bytes: plan.Artifact.Size,
		Consent: consent, By: by, Surface: surface}
	record := func(outcome string, err error) {
		ev.Outcome = outcome
		if err != nil {
			ev.Error = err.Error()
		}
		if s.Record != nil {
			s.Record(ev)
		}
	}
	if s.Off {
		record(agent.ToolchainRefused, ErrOff)
		return Installed{}, ErrOff
	}
	if plan.Installed != nil {
		ev.Cached, ev.URL, ev.Digest, ev.Bytes = true, plan.Installed.URL, plan.Installed.Digest, plan.Installed.Bytes
		record(agent.ToolchainOK, nil)
		return *plan.Installed, nil
	}
	in, err := s.Cache.Install(ctx, s.Source, plan.Artifact, progress)
	switch {
	case err != nil && ctx.Err() != nil:
		record(agent.ToolchainCancelled, err)
		return Installed{}, err
	case err != nil:
		record(agent.ToolchainFailed, err)
		return Installed{}, err
	}
	ev.Bytes = in.Bytes
	record(agent.ToolchainOK, nil)
	return in, nil
}

// Active are the installed toolchains commands in dir get, one per
// language: the newest matching the project's pin, else the newest installed.
func (s *Session) Active(dir string) []Installed {
	if s.Off {
		return nil
	}
	p := s.platform()
	pins := PinsFrom(dir)
	var out []Installed
	for _, l := range Langs {
		spec := Spec{Lang: l.Name}
		if pin, ok := pins[l.Name]; ok {
			spec = pin.Spec
		}
		if in, ok := s.Cache.Best(p, spec); ok {
			out = append(out, in)
		} else if in, ok := s.Cache.Best(p, Spec{Lang: l.Name}); ok && spec.Version != "" {
			// An installed version is better than none; the pin's own is offered when a command misses it.
			out = append(out, in)
		}
	}
	return out
}

// Prelude is the shell lines a command in dir starts with to find the active
// toolchains: their programs first on PATH, and the variables they need.
func (s *Session) Prelude(dir string) string {
	act := s.Active(dir)
	if len(act) == 0 {
		return ""
	}
	var path []string
	vars := map[string]string{}
	for _, in := range act {
		path = append(path, BinDirs(in)...)
		for k, v := range Vars(in) {
			vars[k] = v
		}
	}
	var b strings.Builder
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "export %s=%s; ", k, shellQuote(vars[k]))
	}
	fmt.Fprintf(&b, "export PATH=%s\"${PATH:+:$PATH}\"; ", shellQuote(strings.Join(path, ":")))
	return b.String()
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// NotFound is what a command that could not find program is told, when a
// toolchain Abhed installs provides it; "" otherwise.
func (s *Session) NotFound(dir, program string) string {
	l, ok := ForProgram(program)
	if !ok {
		return ""
	}
	if s.Off {
		return fmt.Sprintf("%s is not installed in this sandbox, and installing toolchains is turned off here (sandbox.toolchains off). Tell the user.", program)
	}
	spec, why := Choose(Spec{Lang: l.Name}, PinsFrom(dir))
	what := l.Display
	if spec.Version != "" {
		what += " " + spec.Version
	}
	return fmt.Sprintf("%s is not installed in this sandbox. Abhed can install %s (%s) from its official release source, "+
		"checked against its published checksum: call the toolchain tool with name %q (the user is asked first), "+
		"then run the command again.", program, what, why, spec.String())
}
