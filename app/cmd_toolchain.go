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

  -platform host|linux|os-arch   whose builds: this host (the process tier), or
                                 linux (the container tier); default host
  -yes                           install without asking (install)

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
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	p, err := platformFlag(*plat)
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 2
	}
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: true})
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
		if fs.NArg() == 0 {
			fmt.Fprintf(stderr, "abhed: toolchain install names a language\n%s", toolchainUsage)
			return 2
		}
		if why := inAgentCommand(); why != "" {
			fmt.Fprintf(stderr, "abhed: toolchain install is refused inside an agent's command (%s); the agent asks with its toolchain tool\n", why)
			return 1
		}
		return toolchainInstall(cfg, tc, fs.Args(), *yes, stdin, stdout, stderr)
	case "remove", "rm":
		if why := inAgentCommand(); why != "" {
			fmt.Fprintf(stderr, "abhed: toolchain remove is refused inside an agent's command (%s)\n", why)
			return 1
		}
		code := 0
		for _, a := range fs.Args() {
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

func toolchainInstall(cfg config.Config, tc *toolchain.Session, specs []string, yes bool, stdin io.Reader, stdout, stderr io.Writer) int {
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
		if plan.Installed != nil {
			fmt.Fprintf(stdout, "%s %s is installed already (%s).\n", l.Display, plan.Version(), plan.Installed.Dir)
			continue
		}
		a := plan.Artifact
		fmt.Fprintf(stdout, "%s %s (%s)\n  from   %s\n  url    %s\n  size   %s\n  check  %s\n", l.Display, a.Version, plan.Why, a.Source, a.URL, sizeText(a.Size), a.Digest)
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
		fmt.Fprintf(stdout, "Installed %s %s, verified, in %s.\n", l.Display, in.Version, in.Dir)
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
