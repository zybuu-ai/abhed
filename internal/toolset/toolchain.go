package toolset

import (
	"context"
	"os/exec"
	"runtime"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/sandbox"
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
	return &toolchain.Session{
		Cache:     toolchain.Cache{Root: root},
		Source:    &toolchain.Source{Endpoints: ToolchainEndpoints, UserAgent: "abhed"},
		Workspace: workspace,
		Platform:  func() toolchain.Platform { return PlatformOf(sb) },
		Off:       cfg.Sandbox.Toolchains == "off",
	}
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
