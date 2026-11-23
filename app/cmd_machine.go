package app

import (
	"fmt"
	"io"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
)

// machineCmd lists and forgets what the person let Abhed use of their own
// machine setup: kubeconfig contexts, ~/.ssh/config hosts and the cluster
// logins kept from a pasted token. Granting happens only at an approval.
func machineCmd(args []string, out, errw io.Writer) int {
	usage := func() int {
		fmt.Fprintln(errw, "usage: abhed machine list | forget KIND NAME\n"+
			"  KIND is k8s-context, ssh-host or k8s-login. Grants are kept in your own state,\n"+
			"  never in a workspace, and each is given at an approval (\"always for …\").\n"+
			"  Forgetting a k8s-login keeps its stored secret; remove that with abhed secret rm.")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "list":
		gs, err := machinegrant.List()
		if err != nil {
			fmt.Fprintf(errw, "abhed: %v\n", err)
			return 1
		}
		if len(gs) == 0 {
			fmt.Fprintln(out, "nothing of this machine's setup is kept for Abhed")
			return 0
		}
		for _, g := range gs {
			fmt.Fprintf(out, "%-12s %-24s %s (since %s)\n", g.Kind, g.Name, g.What, g.GrantedAt.Format("2006-01-02"))
		}
		return 0
	case "forget", "rm", "remove":
		if len(args) != 3 || !machinegrant.ValidKind(args[1]) {
			return usage()
		}
		found, err := machinegrant.Forget(args[1], args[2])
		switch {
		case err != nil:
			fmt.Fprintf(errw, "abhed: %v\n", err)
			return 1
		case !found:
			fmt.Fprintf(errw, "abhed: no %s %q is kept\n", args[1], args[2])
			return 1
		}
		fmt.Fprintf(out, "forgot %s %s; its next use asks again\n", args[1], args[2])
		return 0
	}
	return usage()
}
