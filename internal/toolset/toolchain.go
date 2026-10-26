package toolset

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// ToolchainEndpoints are the release sources installs use: the official
// ones; a test points them at a fake.
var ToolchainEndpoints = toolchain.Official

// Toolchains is the session toolchains a surface's commands get: the
// per-user cache, the official sources, and the platform commands run on,
// which a container sees as Linux. Nil when the cache has no home.
func Toolchains(cfg config.Config, workspace string, sb sandbox.Sandbox) *toolchain.Session {
	root, err := toolchain.DefaultRoot()
	if err != nil {
		return nil
	}
	tc := &toolchain.Session{
		Cache:     toolchain.Cache{Root: root},
		Source:    &toolchain.Source{Endpoints: ToolchainEndpoints, UserAgent: "abhed"},
		Workspace: workspace,
		Platform:  func() toolchain.Platform { return PlatformOf(sb) },
		Off:       cfg.Sandbox.Toolchains == "off",
	}
	if sandboxconfig.RegistriesOpenable(cfg) {
		tc.Registries = func(lang string) ([]string, string) { return registriesFor(cfg, sb, lang) }
		tc.Open = func(lang string) ([]string, error) {
			hosts, why := registriesFor(cfg, sb, lang)
			if why != "" {
				return nil, errors.New(why)
			}
			if len(hosts) == 0 {
				return nil, nil
			}
			return hosts, sandbox.EgressOf(resolved(sb)).Grant("toolchain "+lang, hosts)
		}
	}
	return tc
}

// registriesFor are a language's registries not yet open to the sandbox's
// commands, or why none can be opened.
func registriesFor(cfg config.Config, sb sandbox.Sandbox, lang string) ([]string, string) {
	if !sandboxconfig.RegistriesOpenable(cfg) {
		if cfg.Sandbox.ToolchainsRegistries == "off" {
			return nil, "sandbox.toolchains_registries is off"
		}
		return nil, "the managed configuration holds the network and does not name sandbox.toolchains_registries"
	}
	if cfg.Sandbox.AllowNetwork && cfg.Sandbox.Network == "" {
		return nil, "" // the network is open already
	}
	pol := sandbox.EgressOf(resolved(sb))
	if pol == nil {
		return nil, "this sandbox cannot hold the egress proxy they are reached through (a container tier on a VM needs control_plane relay_binary), so commands have no network"
	}
	var need []string
	for _, h := range toolchain.Registries[lang] {
		if !pol.Granted(h) {
			need = append(need, h)
		}
	}
	return need, ""
}

// resolved is the sandbox a lazy one chose, waiting for the choice.
func resolved(sb sandbox.Sandbox) sandbox.Sandbox {
	if l, ok := sb.(interface {
		Resolve() (sandbox.Sandbox, error)
	}); ok {
		if r, err := l.Resolve(); err == nil {
			return r
		}
		return nil
	}
	return sb
}

// PlatformOf is the platform a sandbox's commands run on: Linux for a
// container or a VM, else this host.
func PlatformOf(sb sandbox.Sandbox) toolchain.Platform {
	if sb != nil {
		switch sb.Tier() {
		case sandbox.TierContainer, sandbox.TierVM:
			return toolchain.Platform{OS: "linux", Arch: runtime.GOARCH}
		}
	}
	return toolchain.Host()
}

// withToolchains gives bash the active toolchains: their programs first on
// PATH, inside the sandbox, and a command that cannot find one told how to
// get it. A git command run on the host for the sandbox is passed on as it is.
func withToolchains(b tools.Bash, tc *toolchain.Session) tools.Bash {
	if tc == nil {
		return b
	}
	b.NotFound = tc.NotFound
	if tc.Registries != nil && tc.Open != nil && !tc.Off {
		b.NetworkNote = "If it was a language's package registry (" + strings.Join(toolchain.AllRegistries()[:6], ", ") +
			"…), call the toolchain tool with that language: the user is asked, and its registries are opened to commands where this sandbox allows."
	}
	if b.Shell != nil {
		shell := b.Shell
		forward := b.Forward
		b.Shell = func(ctx context.Context, cwd string) *exec.Cmd {
			cmd := shell(ctx, cwd)
			if cmd.Err != nil {
				return cmd
			}
			if cmd.Env == nil {
				cmd.Env = os.Environ()
			}
			// bash runs PROMPT_COMMAND before its first prompt, after any start-up file.
			cmd.Env = append(cmd.Env, "ABHED_SHELL_SETUP="+tc.ShellSetup(cwd),
				`PROMPT_COMMAND=eval "$ABHED_SHELL_SETUP"; unset ABHED_SHELL_SETUP PROMPT_COMMAND`)
			names := []string{"ABHED_SHELL_SETUP", "PROMPT_COMMAND"}
			if forward != nil {
				forward(cmd, names)
			} else {
				sandbox.ForwardEnv(cmd, names)
			}
			return cmd
		}
	}
	if tc.Off || b.Sandbox == nil {
		return b
	}
	inner := b.Sandbox
	b.Sandbox = func(ctx context.Context, cwd, line string) *exec.Cmd {
		if hostgit.Brokered(line) {
			return inner(ctx, cwd, line)
		}
		return inner(ctx, cwd, tc.Prelude(cwd)+line)
	}
	return b
}

// ToolchainAllows are the allow rules sandbox.toolchains auto and
// sandbox.toolchains_preapproved add: an install they cover needs no ask.
func ToolchainAllows(cfg config.Config) []string {
	// Where the managed file holds the permissions, only its own say widens them.
	if cfg.AllowLocked() && !cfg.ManagedSets("sandbox.toolchains") && !cfg.ManagedSets("sandbox.toolchains_preapproved") {
		return nil
	}
	switch cfg.Sandbox.Toolchains {
	case "off":
		return nil
	case "auto":
		return []string{toolchain.ToolName}
	}
	var out []string
	for _, p := range cfg.Sandbox.ToolchainsPreapproved {
		spec, err := toolchain.ParseSpec(p)
		if err != nil {
			continue
		}
		if spec.Version == "" {
			out = append(out, toolchain.ToolName+"("+spec.Lang+"@*)")
		} else {
			out = append(out, toolchain.ToolName+"("+spec.Lang+"@"+toolchain.Major(spec.Lang, spec.Version)+")")
		}
	}
	return out
}
