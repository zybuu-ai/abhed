package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

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
	// typed then is queued until the turn ends.
	MidTurn bool
	// ReadOnly commands change nothing in the session or the workspace.
	ReadOnly bool
	// Source is builtin for registered commands; a slashSource sets its own.
	Source string
	Run    func(ctx context.Context, e *cmdEnv, args []string) (quit bool, err error)
}

// slashSource supplies commands found while running, such as custom command
// files or MCP prompts. It is asked again on each lookup, so it may change.
type slashSource interface {
	SlashCommands() []slashCmd
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
// that claims to be built in is refused.
func (g *slashRegistry) lookup(name string, dynamic []slashSource) (slashCmd, bool) {
	g.mu.RLock()
	c, ok := g.byName[name]
	g.mu.RUnlock()
	if ok {
		return *c, true
	}
	for _, d := range g.admitted(dynamic) {
		if d.Name == name || slices.Contains(d.Aliases, name) {
			return d, true
		}
	}
	return slashCmd{}, false
}

// admitted is the run-time commands that may run: not built in by claim, not
// shadowing a built-in, and the first of any that share a name.
func (g *slashRegistry) admitted(dynamic []slashSource) []slashCmd {
	g.mu.RLock()
	defer g.mu.RUnlock()
	taken := map[string]bool{}
	var out []slashCmd
	for _, src := range dynamic {
		for _, d := range src.SlashCommands() {
			names := append([]string{d.Name}, d.Aliases...)
			ok := d.Source != "" && d.Source != sourceBuiltin && d.Run != nil && strings.HasPrefix(d.Name, "/")
			for _, n := range names {
				if _, builtin := g.byName[n]; builtin || taken[n] {
					ok = false
				}
			}
			if !ok {
				continue
			}
			for _, n := range names {
				taken[n] = true
			}
			out = append(out, d)
		}
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
	extra := g.admitted(dynamic)
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
	env := &cmdEnv{ui: st.surface, r: r, st: st, pol: pol, sess: sess, dynamic: st.dynamic}
	if env.ui == nil {
		env.ui = ui.NewLineSurface(ui.LazyStdout{}, s, nil)
	}

	c, ok := builtinSlash.lookup(fields[0], env.dynamic)
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
