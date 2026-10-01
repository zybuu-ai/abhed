package app

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// The registry lists the commands exactly as the static list did before it,
// so /help, completion and the menu are unchanged by the move. /cost is now
// an alias of /usage, which takes its place.
func TestRegistryKeepsTheHelpList(t *testing.T) {
	want := []ui.Command{
		{Name: "/mode", Args: "<name>", Help: "default | accept-edits | plan | auto"},
		{Name: "/undo", Help: "revert the last turn's file changes"},
		{Name: "/diff", Help: "files changed this session"},
		{Name: "/usage", Help: "tokens, cache hit rate, prefill saving, and by subagent and tool source"},
		{Name: "/compact", Args: "[focus]", Help: "compact the context now, keeping what focus names"},
		{Name: "/clear", Args: "[name]", Help: "end this session and start a new one; nothing is deleted"},
		{Name: "/tasks", Args: "[view|kill <n> | cancel <id|all>]", Help: "list subagents and background tasks, view or stop one"},
		{Name: "/wake", Args: "[off|notify|auto]", Help: "show or set what a background result does while idle"},
		{Name: "/memory", Args: "[show <n>|add <scope> <note>|auto on|off]", Help: "show the ABHED.md files in effect"},
		{Name: "/model", Args: "[name]", Help: "show or switch the model, keeping the conversation"},
		{Name: "/sessions", Help: "list this workspace's recorded sessions"},
		{Name: "/resume", Args: "[id|name]", Help: "replay a past session and continue its conversation; alone, pick one"},
		{Name: "/tree", Help: "show the session's steps, with the numbers /fork takes"},
		{Name: "/fork", Args: "[step]", Help: "rebuild the conversation up to a step and continue from it"},
		{Name: "/export", Args: "[path]", Help: "write the transcript to ~/.abhed/exports (.html; .jsonl verifiable, .json, .txt)"},
		{Name: "/hawkeye", Args: "[path]", Help: "what this session did: tokens, policy decisions, findings"},
		{Name: "/think", Help: "show or collapse the model's reasoning"},
		{Name: "/cwd", Help: "show the workspace root"},
		{Name: "/help", Help: "this list"},
		{Name: "/quit", Help: "exit"},
	}
	// Commands added later may sit anywhere; these keep their order and text.
	var got []ui.Command
	for _, c := range builtinSlash.uiCommands(nil) {
		if slices.ContainsFunc(want, func(w ui.Command) bool { return w.Name == c.Name }) {
			got = append(got, c)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the registry changed the command list:\n got %v\nwant %v", got, want)
	}
	if !slices.Equal(ui.CommandList(), builtinSlash.uiCommands(nil)) {
		t.Fatal("the ui shows a list other than the registry's")
	}
}

type fakeSource struct {
	kind string
	cmds []slashCmd
}

func (f fakeSource) Source() string            { return f.kind }
func (f fakeSource) SlashCommands() []slashCmd { return f.cmds }

func runs(name string, ran *string) func(context.Context, *cmdEnv, []string) (bool, error) {
	return func(context.Context, *cmdEnv, []string) (bool, error) { *ran = name; return false, nil }
}

// A run-time command can never take a built-in's name or alias; of two that
// share a name, the first source wins.
func TestBuiltinNamesAlwaysWin(t *testing.T) {
	var ran string
	user := fakeSource{sourceUser, []slashCmd{
		{Name: "/mode", Run: runs("user /mode", &ran)},
		{Name: "/leave", Aliases: []string{"/exit"}, Run: runs("user /leave", &ran)},
		{Name: "/deploy", Args: "[env]", Help: "ship it", Run: runs("user /deploy", &ran)},
	}}
	mcp := fakeSource{sourceMCP, []slashCmd{{Name: "/deploy", Run: runs("mcp /deploy", &ran)}}}
	dyn := []slashSource{user, mcp}

	c, _, ok := builtinSlash.lookup("/mode", dyn)
	if !ok || c.Source != sourceBuiltin {
		t.Fatalf("/mode resolved to %+v", c)
	}
	if c, _, _ := builtinSlash.lookup("/exit", dyn); c.Name != "/quit" {
		t.Fatalf("/exit resolved to %s, not the built-in /quit", c.Name)
	}
	if _, why, ok := builtinSlash.lookup("/leave", dyn); ok || !strings.Contains(why, "built-in /exit") {
		t.Errorf("/leave: admitted %v, why %q", ok, why)
	}
	c, _, ok = builtinSlash.lookup("/deploy", dyn)
	if !ok || c.Source != sourceUser {
		t.Fatalf("/deploy resolved to %+v, want the first source's", c)
	}

	list := builtinSlash.uiCommands(dyn)
	names := map[string]int{}
	for _, c := range list {
		names[c.Name]++
	}
	if names["/mode"] != 1 || names["/deploy"] != 1 || names["/leave"] != 0 {
		t.Fatalf("listed %v", names)
	}
	if list[len(list)-1].Name != "/deploy" {
		t.Fatalf("run-time commands should follow the built-ins: %v", list)
	}

	// Through the dispatcher: the built-in runs, the run-time one only by its own name.
	st := &cliState{dynamic: dyn}
	r := ui.NewRenderer(io.Discard, false)
	pol := policy.New(policy.ModeDefault)
	if !handleCommand(context.Background(), "/exit", r, pol, nil, st) {
		t.Fatal("/exit no longer quits")
	}
	if handleCommand(context.Background(), "/deploy prod", r, pol, nil, st) || ran != "user /deploy" {
		t.Fatalf("ran %q", ran)
	}
	ran = ""
	if handleCommand(context.Background(), "/leave", r, pol, nil, st) || ran != "" {
		t.Fatalf("a command that shadows a built-in alias ran: %q", ran)
	}
}

// The source is the loader's to say: a command cannot record itself as
// managed or built in, and a source of no known kind supplies nothing.
func TestCommandSourceIsSetByTheLoader(t *testing.T) {
	var ran string
	dyn := []slashSource{
		fakeSource{sourceWorkspace, []slashCmd{{Name: "/ship", Source: sourceManaged, Run: runs("ship", &ran)}}},
		fakeSource{sourceBuiltin, []slashCmd{{Name: "/sneaky", Run: runs("sneaky", &ran)}}},
		fakeSource{"", []slashCmd{{Name: "/nosource", Run: runs("nosource", &ran)}}},
		fakeSource{sourceUser, []slashCmd{{Name: "/norun"}}},
	}
	c, _, ok := builtinSlash.lookup("/ship", dyn)
	if !ok || c.Source != sourceWorkspace {
		t.Fatalf("/ship resolved to source %q, want the loader's %q", c.Source, sourceWorkspace)
	}
	for _, name := range []string{"/sneaky", "/nosource", "/norun"} {
		if _, why, ok := builtinSlash.lookup(name, dyn); ok || why == "" {
			t.Errorf("%s: admitted %v, why %q", name, ok, why)
		}
	}
}

// A run-time name that differs from a built-in's only by case, separators or
// look-alike digits, or that uses characters outside plain ASCII, is refused
// and the person is told why rather than left with two look-alike commands.
func TestLookalikeNamesAreRefusedAndSaid(t *testing.T) {
	var ran string
	var cmds []slashCmd
	for _, n := range []string{"/Mode", "/MODE", "/m0de", "/hawk-eye", "/c1ear", "/m\u043ede", "/mode\u200b", "/Exit", "/heIp", "/HELP", "/he1p", "/rnode", "/perrnissions", "/he.lp", "/help.", "/help:", "/vvake", "/deploy"} {
		cmds = append(cmds, slashCmd{Name: n, Run: runs(n, &ran)})
	}
	dyn := []slashSource{fakeSource{sourceWorkspace, cmds}}
	admitted, dropped := builtinSlash.admitted(dyn)
	if len(admitted) != 1 || admitted[0].Name != "/deploy" {
		t.Fatalf("admitted %v", admitted)
	}
	if len(dropped) != len(cmds)-1 {
		t.Fatalf("dropped %v", dropped)
	}
	warnings := slashWarnings(dyn)
	if len(warnings) != len(dropped) || !strings.Contains(warnings[0], "workspace command /Mode is not available: /Mode looks like the built-in /mode") {
		t.Fatalf("warnings %q", warnings)
	}
	var out strings.Builder
	r := ui.NewRenderer(&out, false)
	st := &cliState{dynamic: dyn}
	old := os.Stdout
	rd, w, _ := os.Pipe()
	os.Stdout = w
	handleCommand(context.Background(), "/m0de", r, policy.New(policy.ModeDefault), nil, st)
	w.Close()
	os.Stdout = old
	printed, _ := io.ReadAll(rd)
	if ran != "" || !strings.Contains(string(printed), "/m0de is not available: /m0de looks like the built-in /mode") {
		t.Fatalf("ran %q, printed %q", ran, printed)
	}
}

// Registering a name or alias twice is refused at start-up.
func TestRegisterTwicePanics(t *testing.T) {
	noop := func(context.Context, *cmdEnv, []string) (bool, error) { return false, nil }
	for _, second := range []slashCmd{
		{Name: "/a", Run: noop},
		{Name: "/b", Aliases: []string{"/a"}, Run: noop},
		{Name: "/c", Aliases: []string{"/x"}, Run: noop},
	} {
		g := &slashRegistry{byName: map[string]*slashCmd{}}
		g.register(slashCmd{Name: "/a", Aliases: []string{"/x"}, Run: noop})
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registering %s after /a did not panic", second.Name)
				}
			}()
			g.register(second)
		}()
	}
	g := &slashRegistry{byName: map[string]*slashCmd{}}
	for _, bad := range []slashCmd{{Name: "noslash", Run: noop}, {Name: "/norun"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%+v was registered", bad)
				}
			}()
			g.register(bad)
		}()
	}
}

// The dispatcher: quit words end the session, an unknown name does not, and
// a command's error is shown rather than ending anything.
func TestDispatchQuitUnknownAndErrors(t *testing.T) {
	r := ui.NewRenderer(io.Discard, false)
	pol := policy.New(policy.ModeDefault)
	st := &cliState{}
	for _, line := range []string{"/quit", "/exit", "/quit now"} {
		if !handleCommand(context.Background(), line, r, pol, nil, st) {
			t.Errorf("%s did not quit", line)
		}
	}
	if handleCommand(context.Background(), "/nope", r, pol, nil, st) {
		t.Error("an unknown command quit the session")
	}
	var gotArgs []string
	st.dynamic = []slashSource{fakeSource{sourceUser, []slashCmd{{Name: "/fails",
		Run: func(_ context.Context, e *cmdEnv, args []string) (bool, error) {
			if e.st != st || e.pol != pol || e.r != r {
				t.Error("the command did not get the session's environment")
			}
			gotArgs = args
			return false, errors.New("boom")
		}}}}}
	if handleCommand(context.Background(), "/fails a b", r, pol, nil, st) {
		t.Error("a failing command quit the session")
	}
	if !slices.Equal(gotArgs, []string{"a", "b"}) {
		t.Errorf("args = %v", gotArgs)
	}
}

// Every built-in says what it does and belongs to a group.
func TestBuiltinsAreDescribed(t *testing.T) {
	builtinSlash.mu.RLock()
	defer builtinSlash.mu.RUnlock()
	for _, c := range builtinSlash.all {
		if c.Help == "" || c.Group == "" || c.Order == 0 || c.Source != sourceBuiltin {
			t.Errorf("%s: %+v", c.Name, *c)
		}
	}
}

// Until the terminal UI provides a surface, a command's question is refused
// at once: the prompt's own goroutine runs the command, so waiting for a line
// there would hang the session.
func TestCommandsGetASurfaceThatRefusesQuestions(t *testing.T) {
	var got string
	var gotErr error
	st := &cliState{dynamic: []slashSource{fakeSource{sourceUser, []slashCmd{{Name: "/ask",
		Run: func(ctx context.Context, e *cmdEnv, _ []string) (bool, error) {
			got, gotErr = e.ui.Dialog(ctx, ui.DialogSpec{Kind: ui.DialogConfirm, Title: "sure?"})
			return false, nil
		}}}}}}
	done := make(chan struct{})
	go func() {
		handleCommand(context.Background(), "/ask", ui.NewRenderer(io.Discard, false), policy.New(policy.ModeDefault), nil, st)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the question hung the prompt")
	}
	if got != "" || !errors.Is(gotErr, ui.ErrNoAnswer) {
		t.Fatalf("got %q, %v", got, gotErr)
	}
}

// reentrant registers a command while being asked for its own.
type reentrant struct{ g *slashRegistry }

func (reentrant) Source() string { return sourceMCP }
func (r reentrant) SlashCommands() []slashCmd {
	r.g.register(slashCmd{Name: "/late", Run: func(context.Context, *cmdEnv, []string) (bool, error) { return false, nil }})
	return nil
}

// Sources are code the registry does not control, so they are asked without
// its lock held: one that registers, or blocks, cannot deadlock it.
func TestSourcesAreAskedOutsideTheLock(t *testing.T) {
	g := &slashRegistry{byName: map[string]*slashCmd{}}
	done := make(chan struct{})
	go func() {
		g.admitted([]slashSource{reentrant{g}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("asking a source deadlocked the registry")
	}
}
