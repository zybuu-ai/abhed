package app

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/ui"
)

func init() {
	registerSlash(slashCmd{Name: "/account", Args: "[signout | switch]",
		Help:  "who is working; signout or switch ends this session and the next start asks again",
		Group: "app", Order: 177, Run: slashAccount})
}

// slashAccount is /account: who is working, and the way to change it. A
// switch ends the session: the person's folder, record and memory are fixed
// for a process, and nothing of one person carries into another's session.
func slashAccount(_ context.Context, e *cmdEnv, args []string) (bool, error) {
	p := currentPrincipal()
	if len(args) == 0 {
		lines := []string{"Working as " + p.Display() + "."}
		switch p.Kind {
		case identity.Guest:
			lines = append(lines, "Nothing of this session is kept. /account switch ends it so the next start can sign in.")
		case identity.Legacy:
			lines = append(lines, "Sign-in is not set up yet. /account switch ends this session; the next start offers to create your account and bring your data.")
		default:
			lines = append(lines, "/account signout signs this machine out; /account switch ends this session so the next start asks who is working.")
		}
		for _, l := range lines {
			e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: l})
		}
		return false, nil
	}
	switch args[0] {
	case "signout", "switch":
		if args[0] == "signout" || p.Kind == identity.Account || p.Kind == identity.Server {
			note, err := signOutHere()
			if err != nil {
				return false, err
			}
			if note != "" {
				e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: note})
			}
		}
		e.ui.Append(ui.Block{Kind: ui.BlockNotice, Text: "Ending this session. The next start asks who is working."})
		return true, nil
	}
	return false, fmt.Errorf("unknown /account %q; use /account, /account signout or /account switch", args[0])
}
