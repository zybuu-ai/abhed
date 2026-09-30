package app

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// slashCase is the shape the commands moved out of the old switch still have;
// legacy adapts one to a slashCmd's Run.
type slashCase func(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool

// legacy runs a slashCase as a registered command. It never returns an error:
// those commands print their own.
func legacy(name string, f slashCase) func(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
	return func(ctx context.Context, e *cmdEnv, args []string) (bool, error) {
		fields := append([]string{name}, args...)
		return f(ctx, fields, e.r, e.pol, e.sess, e.st, e.r.Style()), nil
	}
}

// Sources a slash command can come from, as command.invoked records them.
const (
	sourceBuiltin   = "builtin"
	sourceUser      = "user"
	sourceWorkspace = "workspace"
	sourceManaged   = "managed"
	sourceMCP       = "mcp"
)

// slashCmd is one slash command. Built-in commands register from init() in
// the file that owns them; commands found at run time (custom command files,
// MCP prompts) come from a slashSource instead.
type slashCmd struct {
	Name    string   // "/mode"
	Aliases []string // other names that run it, e.g. "/exit"; not listed in /help
	Args    string   // "<name>", "[hint]", ""
	Help    string   // one line for /help and the menu
	Group   string   // session, rewind, context, model, status, background, app
	// Order places the command in /help: lower first, ties by name. Leave
	// gaps so a command can go between two others without renumbering.
	Order int
	// MidTurn commands may run while a turn is in progress; every other one
	// typed then is queued until the turn ends. Not enforced yet: every
	// command typed mid-turn is queued today.
	MidTurn bool
	// ReadOnly commands change nothing in the session or the workspace. Not
	// enforced yet, so nothing may rely on it as a guard.
	ReadOnly bool
	// Source is where the command came from, as command.invoked records it.
	// The registry sets it: builtin for registered commands, and the
	// slashSource's Source for the rest. What a command says is ignored.
	Source string
	Run    func(ctx context.Context, e *cmdEnv, args []string) (quit bool, err error)
}

// slashSource supplies commands found while running, such as custom command
// files or MCP prompts. It is asked again on each lookup, so it may change,
// and never while the registry's lock is held.
type slashSource interface {
	// Source is the kind of every command it supplies: user, workspace,
	// managed or mcp. The loader that builds the source decides it.
	Source() string
	SlashCommands() []slashCmd
}

// runtimeSources are the kinds a slashSource may be.
var runtimeSources = []string{sourceUser, sourceWorkspace, sourceManaged, sourceMCP}

// runtimeName is the shape of a run-time command's name: ASCII only, so no
// character can pass for another from a different script.
var runtimeName = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9_.:-]*$`)

// droppedSlash is a run-time command that was not admitted, and why.
type droppedSlash struct {
	Name, Source, Reason string
}

// cmdEnv is what a command runs against.
type cmdEnv struct {
	// ui is where a command shows and asks things. Until the terminal UI
	// sets cliState.surface it is a line surface with no answers, so any
	// question is refused rather than hanging the prompt.
	ui   ui.Surface
	r    *ui.Renderer
	st   *cliState
	pol  *policy.Engine
	sess *tools.Session
	// modes is how a command changes the permission mode.
	modes ModeController
	// input expands text a command sends as a message.
	input InputExpander
	// dynamic are the run-time command sources, after the built-ins.
	dynamic []slashSource
}

// slashRegistry holds the built-in commands by name and alias.
type slashRegistry struct {
	mu     sync.RWMutex
	byName map[string]*slashCmd
	all    []*slashCmd
}

var builtinSlash = &slashRegistry{byName: map[string]*slashCmd{}}

// registerSlash adds a built-in command. A name or alias registered twice is a
// programming error, so it panics at start-up rather than letting one shadow
// the other.
func registerSlash(c slashCmd) {
	builtinSlash.register(c)
	ui.SetCommands(builtinSlash.uiCommands(nil))
}

func (g *slashRegistry) register(c slashCmd) {
	if !strings.HasPrefix(c.Name, "/") || c.Run == nil {
		panic(fmt.Sprintf("slash command %q needs a /name and a Run", c.Name))
	}
	c.Source = sourceBuiltin
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		if _, dup := g.byName[n]; dup {
			panic("slash command registered twice: " + n)
		}
	}
	p := &c
	for _, n := range append([]string{c.Name}, c.Aliases...) {
		g.byName[n] = p
	}
	g.all = append(g.all, p)
}

// lookup finds the command a typed name runs. Built-in names and aliases
// always win: a run-time command that claims one is never reached, and one
// that claims to be built in is refused. When the name is a run-time command
// that was refused, the reason is returned instead.
func (g *slashRegistry) lookup(name string, dynamic []slashSource) (slashCmd, string, bool) {
	g.mu.RLock()
	c, ok := g.byName[name]
	g.mu.RUnlock()
	if ok {
		return *c, "", true
	}
	admitted, dropped := g.admitted(dynamic)
	for _, d := range admitted {
		if d.Name == name || slices.Contains(d.Aliases, name) {
			return d, "", true
		}
	}
	for _, d := range dropped {
		if d.Name == name {
			return slashCmd{}, d.Reason, false
		}
	}
	return slashCmd{}, "", false
}

// lookalike folds a name so that two a person could mistake for each other
// fold alike: case, - and _, and 0 and 1 for o and l.
func lookalike(name string) string {
	return strings.NewReplacer("-", "", "_", "", "0", "o", "1", "l").Replace(strings.ToLower(name))
}

// admitted is the run-time commands that may run, and those that may not,
// with why: a source of no known kind, a name that is not plain ASCII, one
// that is or looks like a built-in's, or one an earlier source already took.
// The sources are asked outside the lock, since they are code the registry
// does not control.
func (g *slashRegistry) admitted(dynamic []slashSource) ([]slashCmd, []droppedSlash) {
	g.mu.RLock()
	builtin := map[string]string{}
	for n := range g.byName {
		builtin[lookalike(n)] = n
	}
	g.mu.RUnlock()
	taken := map[string]bool{}
	var out []slashCmd
	var dropped []droppedSlash
	for _, src := range dynamic {
		kind := src.Source()
		for _, d := range src.SlashCommands() {
			d.Source = kind
			reason := ""
			names := append([]string{d.Name}, d.Aliases...)
			switch {
			case !slices.Contains(runtimeSources, kind):
				reason = fmt.Sprintf("its source %q is not one a command may come from", kind)
			case d.Run == nil:
				reason = "it has nothing to run"
			}
			for _, n := range names {
				if reason != "" {
					break
				}
				switch b, clash := builtin[lookalike(n)]; {
				case !runtimeName.MatchString(n):
					reason = fmt.Sprintf("%s is not a plain name (letters, digits, - _ . :)", config.Printable(n))
				case clash && b == n:
					reason = "the built-in " + b + " has that name"
				case clash:
					reason = fmt.Sprintf("%s looks like the built-in %s", n, b)
				case taken[n]:
					reason = "an earlier command already has the name " + n
				}
			}
			if reason != "" {
				dropped = append(dropped, droppedSlash{Name: d.Name, Source: kind, Reason: reason})
				continue
			}
			for _, n := range names {
				taken[n] = true
			}
			out = append(out, d)
		}
	}
	return out, dropped
}

// slashWarnings says which run-time commands were refused and why, for the
// loader to show when its sources change.
func slashWarnings(dynamic []slashSource) []string {
	_, dropped := builtinSlash.admitted(dynamic)
	out := make([]string, 0, len(dropped))
	for _, d := range dropped {
		out = append(out, fmt.Sprintf("%s command %s is not available: %s", d.Source, config.Printable(d.Name), d.Reason))
	}
	return out
}

// uiCommands is the list /help, completion and the menu show: the built-ins
// in their order, then the admitted run-time commands by name. A source that
// changes passes it to ui.SetCommands with the session's sources.
func (g *slashRegistry) uiCommands(dynamic []slashSource) []ui.Command {
	g.mu.RLock()
	all := slices.Clone(g.all)
	g.mu.RUnlock()
	slices.SortStableFunc(all, func(a, b *slashCmd) int {
		if a.Order != b.Order {
			return a.Order - b.Order
		}
		return strings.Compare(a.Name, b.Name)
	})
	out := make([]ui.Command, 0, len(all))
	for _, c := range all {
		out = append(out, ui.Command{Name: c.Name, Args: c.Args, Help: c.Help})
	}
	extra, _ := g.admitted(dynamic)
	slices.SortFunc(extra, func(a, b slashCmd) int { return strings.Compare(a.Name, b.Name) })
	for _, c := range extra {
		out = append(out, ui.Command{Name: c.Name, Args: c.Args, Help: c.Help})
	}
	return out
}

func init() {
	registerSlash(slashCmd{Name: "/help", Help: "this list", Group: "app", Order: 190, ReadOnly: true, Run: legacy("/help", slashHelp)})
	registerSlash(slashCmd{Name: "/quit", Aliases: []string{"/exit"}, Help: "exit", Group: "app", Order: 200, ReadOnly: true,
		Run: func(context.Context, *cmdEnv, []string) (bool, error) { return true, nil }})
}

// handleCommand runs one slash command line and reports whether the session
// should end.
func handleCommand(ctx context.Context, line string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState) bool {
	s := r.Style()
	fields := strings.Fields(line)
	env := &cmdEnv{ui: st.surface, r: r, st: st, pol: pol, sess: sess, dynamic: st.dynamic,
		modes: &cliModes{st: st, pol: pol}, input: plainInput{}}
	if env.ui == nil {
		env.ui = ui.NewLineSurface(ui.LazyStdout{}, s, nil)
	}

	c, refused, ok := builtinSlash.lookup(fields[0], env.dynamic)
	if refused != "" {
		fmt.Printf("  %s %s is not available: %s\n", s.Red("✕"), config.Printable(fields[0]), refused)
		return false
	}
	if !ok {
		fmt.Printf("  %s unknown command %s — try /help\n", s.Red("✕"), fields[0])
		return false
	}
	quit, err := c.Run(ctx, env, fields[1:])
	if err != nil {
		fmt.Printf("  %s %v\n", s.Red("✕"), err)
	}
	return quit
}

// slashHelp is /help.
func slashHelp(ctx context.Context, fields []string, r *ui.Renderer,
	pol *policy.Engine, sess *tools.Session, st *cliState, s ui.Style) bool {
	// One list, in internal/ui: help, tab completion and the suggestion
	// menu cannot drift apart if they read the same source.
	fmt.Println(ui.HelpText(s))
	return false
}
