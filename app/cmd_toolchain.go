package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/server"
)

const toolchainUsage = `usage: abhed toolchain <command> [flags] [lang[@version]...]

  install lang[@version]...  download, verify and install a toolchain for the
                             agent's commands; the version is the one given,
                             else the workspace's pin, else the latest stable
  list                       the installed toolchains and the workspace's pins (-json)
  remove lang@version...     remove installed toolchains
  clean                      remove the whole toolchain cache, every toolchain
                             and extra for every platform (-yes, else asked)

  -platform host|linux|os-arch   whose builds: this host (the process tier), or
                                 linux (the container tier); default host
  -yes                           install without asking (install)
  -lsp                           also install the language's language server:
                                 gopls (go), basedpyright (python, on Node.js,
                                 installed first if missing), typescript-language-server
                                 (node), rust-analyzer (rust)
  -debugger                      also install the language's debugger: Delve
                                 for go (Linux builds only), debugpy for python

Languages: ` + "go, python, node, rust, java, maven, gradle, ruby, dotnet" + `
`

func toolchainCmd(workspace string, args []string, trust config.TrustChoice, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		fmt.Fprint(stdout, toolchainUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("toolchain "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print as JSON (list)")
	plat := fs.String("platform", "host", "host, linux, or os-arch")
	yes := fs.Bool("yes", false, "install without asking")
	debugger := fs.Bool("debugger", false, "also install the language's debugger (install)")
	lsp := fs.Bool("lsp", false, "also install the language's language server (install)")
	// Flags may follow the languages too, as abhed doctor writes them; after
	// a "--" every word is a language.
	var positional []string
	for {
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		if n := len(rest) - fs.NArg(); n > 0 && rest[n-1] == "--" {
			positional = append(positional, fs.Args()...)
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	args = positional
	p, err := platformFlag(*plat)
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 2
	}
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: true})
	if err == nil {
		err = cfg.ApplyNetwork()
	}
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 1
	}
	tc := toolset.Toolchains(cfg, workspace, nil)
	if tc == nil {
		fmt.Fprintln(stderr, "abhed: the toolchain cache has no home here")
		return 1
	}
	tc.Platform = func() toolchain.Platform { return p }
	switch verb {
	case "list", "ls":
		return toolchainList(tc, p, *asJSON, stdout)
	case "install":
		if len(args) == 0 {
			fmt.Fprintf(stderr, "abhed: toolchain install names a language\n%s", toolchainUsage)
			return 2
		}
		if why := inAgentCommand(); why != "" {
			fmt.Fprintf(stderr, "abhed: toolchain install is refused inside an agent's command (%s); the agent asks with its toolchain tool\n", why)
			return 1
		}
		return toolchainInstall(cfg, tc, args, *yes, *debugger, *lsp, stdin, stdout, stderr)
	case "clean":
		if why := inAgentCommand(); why != "" {
			fmt.Fprintf(stderr, "abhed: toolchain clean is refused inside an agent's command (%s)\n", why)
			return 1
		}
		if !*yes {
			if !ollamaTerminal(stdin, stdout) {
				fmt.Fprintln(stderr, "abhed: not at a terminal, so nothing was removed; add -yes to remove the toolchain cache without asking")
				return 1
			}
			fmt.Fprintf(stdout, "Remove every toolchain in %s? [y/N] ", tc.Cache.Root)
			if line, _ := readAnswer(stdin); !strings.EqualFold(strings.TrimSpace(line), "y") && !strings.EqualFold(strings.TrimSpace(line), "yes") {
				fmt.Fprintln(stdout, "Nothing was removed.")
				return 0
			}
		}
		if err := tc.Cache.Clean(); err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed the toolchain cache %s\n", tc.Cache.Root)
		return 0
	case "remove", "rm":
		if why := inAgentCommand(); why != "" {
			fmt.Fprintf(stderr, "abhed: toolchain remove is refused inside an agent's command (%s)\n", why)
			return 1
		}
		code := 0
		for _, a := range args {
			spec, err := toolchain.ParseSpec(a)
			if err == nil && spec.Version == "" {
				err = fmt.Errorf("name the version to remove, such as %s@1.22.5", spec.Lang)
			}
			if err == nil {
				if in, ok := tc.Cache.Best(p, spec); ok {
					err = tc.Cache.Remove(p, in.Lang, in.Version)
					if err == nil {
						fmt.Fprintf(stdout, "removed %s %s (%s)\n", in.Lang, in.Version, p)
					}
				} else {
					err = fmt.Errorf("%s is not installed for %s", spec, p)
				}
			}
			if err != nil {
				fmt.Fprintf(stderr, "abhed: %v\n", err)
				code = 1
			}
		}
		return code
	}
	fmt.Fprintf(stderr, "abhed: unknown toolchain command %q\n%s", verb, toolchainUsage)
	return 2
}

func platformFlag(v string) (toolchain.Platform, error) {
	switch v {
	case "", "host":
		return toolchain.Host(), nil
	case "linux", "container":
		return toolchain.Platform{OS: "linux", Arch: runtime.GOARCH}, nil
	}
	os, arch, ok := strings.Cut(v, "-")
	if !ok || os != "linux" && os != "darwin" || arch == "" || strings.ContainsAny(arch, "/.\\") {
		return toolchain.Platform{}, fmt.Errorf("-platform is host, linux, or os-arch such as linux-arm64, not %q", v)
	}
	return toolchain.Platform{OS: os, Arch: arch}, nil
}

func toolchainList(tc *toolchain.Session, p toolchain.Platform, asJSON bool, stdout io.Writer) int {
	list := tc.Cache.List(p, "")
	pins := toolchain.PinsFrom(tc.Workspace)
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"platform": p.String(), "root": tc.Cache.Root, "installed": list, "pins": pins})
		return 0
	}
	if len(list) == 0 {
		fmt.Fprintf(stdout, "No toolchains are installed for %s (%s).\n", p, tc.Cache.Root)
	}
	for _, in := range list {
		fmt.Fprintf(stdout, "%-7s %-10s %s  %s\n", in.Lang, in.Version, in.Digest[:min(len(in.Digest), 19)]+"…", in.Dir)
	}
	for lang, pin := range pins {
		fmt.Fprintf(stdout, "%s pins %s %s\n", pin.File, lang, pin.Spec.Version)
	}
	return 0
}

func toolchainInstall(cfg config.Config, tc *toolchain.Session, specs []string, yes, debugger, lsp bool, stdin io.Reader, stdout, stderr io.Writer) int {
	ctx, stop := signalContext()
	defer stop()
	rec := &toolchainRecord{ctx: ctx, cfg: cfg, out: stderr}
	defer rec.close()
	tc.Record = rec.record
	terminal := ollamaTerminal(stdin, stdout)
	code := 0
	for _, a := range specs {
		spec, err := toolchain.ParseSpec(a)
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %v\n", err)
			code = 2
			continue
		}
		plan, err := tc.Plan(ctx, spec)
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %s: %v\n", spec, err)
			code = 1
			continue
		}
		l, _ := toolchain.Find(plan.Spec.Lang)
		var extras []*toolchain.ExtraPlan
		failed := false
		for _, want := range []struct {
			on   bool
			plan func(context.Context, *toolchain.Session, toolchain.Plan) (toolchain.ExtraPlan, error)
		}{{debugger, debuggerPlan}, {lsp, lspPlan}} {
			if !want.on {
				continue
			}
			d, err := want.plan(ctx, tc, plan)
			if err != nil {
				fmt.Fprintf(stderr, "abhed: %s: %v\n", spec, err)
				failed = true
				break
			}
			extras = append(extras, &d)
		}
		if failed {
			code = 1
			continue
		}
		allThere := plan.Installed != nil
		for _, ep := range extras {
			allThere = allThere && ep.Installed != nil
		}
		if allThere {
			fmt.Fprintf(stdout, "%s %s is installed already (%s).\n", l.Display, plan.Version(), plan.Installed.Dir)
			continue
		}
		a := plan.Artifact
		if plan.Installed != nil {
			fmt.Fprintf(stdout, "%s %s is installed already (%s).\n", l.Display, plan.Version(), plan.Installed.Dir)
		} else {
			fmt.Fprintf(stdout, "%s %s (%s)\n  from   %s\n  url    %s\n  size   %s\n  check  %s\n", l.Display, a.Version, plan.Why, a.Source, a.URL, sizeText(a.Size), a.Digest)
		}
		for _, ep := range extras {
			if ep.Installed != nil {
				continue
			}
			if n := ep.Needs; n != nil {
				nl, _ := toolchain.Find(n.Spec.Lang)
				fmt.Fprintf(stdout, "+ %s %s, which it runs on\n  from   %s\n  url    %s\n  size   %s\n  check  %s\n",
					nl.Display, n.Version(), n.Artifact.Source, n.Artifact.URL, sizeText(n.Artifact.Size), n.Artifact.Digest)
			}
			fmt.Fprintf(stdout, "+ %s %s\n  from   %s\n  url    %s\n  check  %s\n", ep.Extra.Display, ep.Extra.Version, ep.Source, ep.URL, ep.Digest)
			if ep.Note != "" {
				fmt.Fprintf(stdout, "  how    %s\n", ep.Note)
			}
		}
		consent := "cli -yes"
		if !yes {
			if !terminal {
				fmt.Fprintln(stderr, "abhed: not at a terminal, so nothing was installed; add -yes to install without asking")
				code = 1
				continue
			}
			fmt.Fprint(stdout, "Install it? [y/N] ")
			line, _ := readAnswer(stdin)
			if !strings.EqualFold(strings.TrimSpace(line), "y") && !strings.EqualFold(strings.TrimSpace(line), "yes") {
				rec.record(agent.ToolchainAction{Lang: a.Lang, Version: a.Version, Platform: a.Platform.String(), Outcome: agent.ToolchainDeclined,
					Why: plan.Why, Source: a.Source, URL: a.URL, Digest: a.Digest, Bytes: a.Size, By: agent.ByUser, Surface: "cli"})
				fmt.Fprintln(stdout, "Nothing was installed.")
				continue
			}
			consent = "cli"
		}
		last := -1
		in, err := tc.Install(ctx, plan, agent.ByUser, "cli", consent, func(done, total int64) {
			if total > 0 && terminal {
				if pct := int(done * 100 / total); pct/10 != last/10 {
					last = pct
					fmt.Fprintf(stdout, "\r  %3d%%", pct)
				}
			}
		})
		if terminal && last >= 0 {
			fmt.Fprintln(stdout)
		}
		if err != nil {
			fmt.Fprintf(stderr, "abhed: %s %s: %v\n", plan.Spec.Lang, a.Version, err)
			code = 1
			continue
		}
		if plan.Installed == nil {
			fmt.Fprintf(stdout, "Installed %s %s, verified, in %s.\n", l.Display, in.Version, in.Dir)
		}
		for _, ep := range extras {
			x, err := tc.InstallExtra(ctx, *ep, in, agent.ByUser, "cli", consent, nil)
			if err != nil {
				fmt.Fprintf(stderr, "abhed: %s %s: %v\n", ep.Extra.Name, ep.Extra.Version, err)
				code = 1
				break
			}
			fmt.Fprintf(stdout, "Installed %s %s (%s) in %s.\n", ep.Extra.Display, x.Version, x.Digest, x.Dir)
		}
	}
	return code
}

func sizeText(n int64) string {
	if n <= 0 {
		return "not published"
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

// toolchainRecord is the short session `abhed toolchain install` records
// its attempts in, opened at the first one.
type toolchainRecord struct {
	ctx   context.Context
	cfg   config.Config
	store server.EventStore
	done  func()
	rec   *agent.Recorder
	id    string
	out   io.Writer
}

func (o *toolchainRecord) record(ev agent.ToolchainAction) {
	if o.rec == nil {
		store, done, err := openStore(o.ctx, o.cfg)
		if err != nil {
			fmt.Fprintf(o.out, "abhed: the record could not be opened, so this is not recorded: %v\n", err)
			return
		}
		id := newConversationID()
		if err := recordSession(o.ctx, store, id, o.cfg); err != nil {
			done()
			return
		}
		o.store, o.done, o.id = store, done, id
		o.rec = agent.NewRecorder(store, id, "")
		if _, err := o.rec.Record(agent.EvSessionStarted, agent.ActorSystem, agent.Trusted, map[string]any{"surface": "abhed toolchain"}); err != nil {
			fmt.Fprintf(o.out, "abhed: recording the session start: %v\n", err)
		}
	}
	if _, err := o.rec.Record(agent.EvToolchainInstall, agent.ActorUser, agent.Trusted, ev); err != nil {
		fmt.Fprintf(o.out, "abhed: could not record %s: %v\n", agent.EvToolchainInstall, err)
	}
}

func (o *toolchainRecord) close() {
	if o.rec == nil {
		return
	}
	if _, err := o.rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted}); err != nil {
		fmt.Fprintf(o.out, "abhed: recording the session end: %v\n", err)
	}
	fmt.Fprintf(o.out, "Recorded as session %s (abhed hawkeye %s reports it).\n", o.id, o.id)
	o.done()
}
