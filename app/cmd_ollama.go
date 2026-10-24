package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ollama"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/server"
)

// abhed ollama: the same check and offers as choosing the model in a
// session, from a terminal of its own. It is the one place the official
// Linux install script runs, since it may ask for a password. Attempts are
// recorded in a short session of their own in the local record, which
// `abhed sessions` lists and `abhed hawkeye` reports on.

const ollamaUsage = `usage: abhed ollama <command> [provider]

  check [provider]   check the provider's local Ollama and offer each missing step:
                     start the server, install Ollama, pull the model (asks first)
  install [provider] the same as check
  status [provider]  say what is found, and the steps, without offering any (-json)
  models             the curated models, with their download sizes
  forget             take back "always start Ollama without asking"

The provider is a configured name; the default is model.default.
`

func ollamaCmd(workspace string, args []string, trust config.TrustChoice, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		fmt.Fprint(stdout, ollamaUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("ollama "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON (status)")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	switch verb {
	case "models":
		for _, c := range ollama.Catalog {
			fmt.Fprintf(stdout, "%-18s %-16s %8s download, wants %d GB of memory\n", c.Tag, c.Label, ollama.HumanBytes(c.Bytes), c.MinRAMGB)
		}
		fmt.Fprintln(stdout, "Sizes are the registry's when the list was made; a pull shows the size it is now.")
		return 0
	case "forget":
		if err := ollamaEnv().SetStartAlways(false); err != nil {
			fmt.Fprintf(stderr, "abhed: ollama forget: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "Abhed will ask again before it starts Ollama.")
		return 0
	case "check", "install", "status":
	default:
		fmt.Fprintf(stderr, "abhed: unknown ollama command %q\n%s", verb, ollamaUsage)
		return 2
	}
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust, Quiet: true})
	if err != nil {
		fmt.Fprintf(stderr, "abhed: %v\n", err)
		return 1
	}
	name := cfg.Model.Default
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if cfg.ModelAllowed(name) != nil {
		fmt.Fprintf(stderr, "abhed: no provider %q is configured\n", name)
		return 2
	}
	t, ok := toolset.OllamaTarget(cfg, name)
	if !ok {
		fmt.Fprintf(stdout, "%s is not served by Ollama; nothing to check.\n", name)
		return 0
	}
	ctx, stop := signalContext()
	defer stop()
	if verb == "status" {
		a := toolset.OllamaAssist(cfg, ollamaEnv(), "cli", true, inAgentCommand(), nil)
		r := a.Report(ctx, t)
		switch {
		case *asJSON:
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(r)
		case r.Problem == nil:
			fmt.Fprintf(stdout, "%s (%s) is ready at %s.\n", name, t.Model, t.API)
		default:
			fmt.Fprintln(stdout, r.Problem.Text())
		}
		if r.Ready {
			return 0
		}
		return 1
	}
	if why := inAgentCommand(); why != "" {
		fmt.Fprintf(stderr, "abhed: ollama %s is refused inside an agent's command (%s)\n", verb, why)
		return 1
	}
	rec := newOllamaRecord(ctx, cfg, t, stderr)
	defer rec.close()
	a := toolset.OllamaAssist(cfg, ollamaEnv(), "cli", true, "", rec.record)
	sf := ui.NewLineSurface(stdout, ui.NewStyle(stdout), readerAnswers{in: stdin})
	f := ollamaFlow{ui: sf, a: a, terminal: true, out: stdout}
	if f.ensure(ctx, t) {
		fmt.Fprintf(stdout, "%s (%s) is ready at %s.\n", name, t.Model, t.API)
		return 0
	}
	return 1
}

// readerAnswers reads a dialog's answers from a reader, a line at a time.
type readerAnswers struct{ in io.Reader }

func (r readerAnswers) Await(ctx context.Context) (string, bool) {
	if ctx.Err() != nil {
		return "", false
	}
	line, err := readAnswer(r.in)
	if err != nil {
		return "", false
	}
	return line, true
}

// signalContext ends at Ctrl-C, so a pull can be stopped.
func signalContext() (context.Context, func()) {
	s := cancelOnStop(stopReturns)
	return s.ctx, s.stop
}

// ollamaRecord is the short session `abhed ollama` records its attempts in,
// opened at the first one.
type ollamaRecord struct {
	ctx   context.Context
	cfg   config.Config
	t     ollama.Target
	store server.EventStore
	done  func()
	rec   *agent.Recorder
	id    string
	out   io.Writer
}

func newOllamaRecord(ctx context.Context, cfg config.Config, t ollama.Target, out io.Writer) *ollamaRecord {
	return &ollamaRecord{ctx: ctx, cfg: cfg, t: t, out: out}
}

func (o *ollamaRecord) record(typ agent.EventType, ev agent.OllamaAction) {
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
		if _, err := o.rec.Record(agent.EvSessionStarted, agent.ActorSystem, agent.Trusted, map[string]any{
			"surface": "abhed ollama", "provider": o.t.Provider, "model": o.t.Model,
		}); err != nil {
			fmt.Fprintf(o.out, "abhed: recording the session start: %v\n", err)
		}
	}
	if _, err := o.rec.Record(typ, agent.ActorUser, agent.Trusted, ev); err != nil {
		fmt.Fprintf(o.out, "abhed: could not record %s: %v\n", typ, err)
	}
}

func (o *ollamaRecord) close() {
	if o.rec == nil {
		return
	}
	if _, err := o.rec.Record(agent.EvSessionEnded, agent.ActorSystem, agent.Trusted, agent.SessionEnded{Reason: agent.TermCompleted}); err != nil {
		fmt.Fprintf(o.out, "abhed: recording the session end: %v\n", err)
	}
	fmt.Fprintf(o.out, "Recorded as session %s (abhed hawkeye %s reports it).\n", o.id, o.id)
	o.done()
}
