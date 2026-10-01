package app

import (
	"context"
	"errors"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// setupTerminal connects the renderer to the input dock, so replies stream
// into it, and gives the dock what it keeps between sessions: the
// workspace's prompt history.
func setupTerminal(editor *ui.LineReader, r *ui.Renderer, workspace string) {
	r.SetWorkspace(workspace)
	if !editor.Raw() {
		return
	}
	r.Attach(editor)
	// A vault secret in a prompt is withheld from the history file as it is
	// from the record.
	h := ui.LoadHistory(ui.HistoryPath(workspace))
	red := openVault().Redactor()
	h.SetRedact(func(s string) string { return string(red.Redact([]byte(s))) })
	editor.SetHistory(h)
}

// dialogApprover asks on the dock's dialog when there is a terminal, and
// through ap's line prompt otherwise. Files are read through the session,
// under its roots and state protection, for the diff an edit would make.
func dialogApprover(ap *ui.Approver, editor *ui.LineReader, r *ui.Renderer, sess *tools.Session) agent.Approver {
	if !editor.Raw() {
		return ap
	}
	return &ui.DialogApprover{Base: ap, Reader: editor, Render: r, ReadFile: sess.ReadFile}
}

func init() {
	registerSlash(slashCmd{Name: "/theme", Args: "[dark|light|high-contrast|colorblind|auto]", Help: "show or set the colour theme",
		Group: "app", Order: 170, MidTurn: true, ReadOnly: true, Run: slashTheme})
	registerSlash(slashCmd{Name: "/vim", Help: "turn vim-style editing of the prompt on or off",
		Group: "app", Order: 175, MidTurn: true, ReadOnly: true, Run: slashVim})
}

// slashTheme is /theme: with no argument it names the theme; with one it
// switches, redraws the screen in it and saves it for the next session.
func slashTheme(_ context.Context, e *cmdEnv, args []string) (bool, error) {
	if len(args) == 0 {
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "theme: " + ui.Theme() + " (" + strings.Join(append(ui.ThemeNames, "auto"), ", ") + ")"})
		return false, nil
	}
	name := args[0]
	chosen := name
	if name == "auto" {
		if chosen = ui.ThemeFromEnv(); chosen == "" {
			chosen = "dark"
		}
	}
	if err := ui.SetTheme(chosen); err != nil {
		return false, err
	}
	_, vim := ui.LoadPrefs()
	if err := ui.SavePrefs(name, vim); err != nil {
		e.ui.Append(ui.Block{Kind: ui.BlockError, Text: "theme not saved: " + err.Error()})
	}
	if l, ok := e.ui.(*ui.LineReader); ok {
		if name == "auto" {
			ui.ForgetTerminal() // and ask the terminal again; its answer applies
		}
		l.SetAutoTheme(name == "auto")
		l.Repaint()
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "theme: " + ui.Theme()})
	return false, nil
}

// slashVim is /vim: vim-style editing of the prompt, on or off, saved.
func slashVim(_ context.Context, e *cmdEnv, _ []string) (bool, error) {
	l, ok := e.ui.(*ui.LineReader)
	if !ok {
		return false, errors.New("vim editing needs a terminal")
	}
	on := !l.Vim()
	l.SetVim(on)
	theme, _ := ui.LoadPrefs()
	_ = ui.SavePrefs(theme, on)
	state := "off"
	if on {
		state = "on: Esc for normal mode, i to type"
	}
	e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "vim editing " + state})
	return false, nil
}
