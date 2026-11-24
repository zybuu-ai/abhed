package app

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/toolset"
	"github.com/zybuu-ai/abhed/internal/ui"
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

// machineDoctor is what doctor says of the person's own machine setup:
// counts and names, servers and addresses, never a credential.
func machineDoctor(cfg config.Config) []string {
	var out []string
	k8sOn, _ := toolset.OrgK8s(cfg)
	off := cfg.MachineSetupOff("k8s")
	switch {
	case cfg.MachineSetupOff("k8s") != "" && !cfg.ManagedSets("k8s.kubeconfig") && !cfg.FromOrg("k8s.kubeconfig"):
		out = append(out, "kubeconfig  not used: "+cfg.MachineSetupOff("k8s"))
	case cfg.Sets("k8s.enabled") && !cfg.K8s.Enabled:
		out = append(out, "kubeconfig  not offered: k8s.enabled is false")
	case toolset.MachineK8s(cfg, true) || k8sOn:
		list, _, err := k8s.Contexts(k8s.Config{Kubeconfig: cfg.K8s.Kubeconfig})
		if err != nil {
			out = append(out, "kubeconfig  none read: "+err.Error())
			break
		}
		names := make([]string, 0, len(list))
		for _, c := range list {
			n := c.Name + " → " + k8s.DisplayURL(c.Server)
			if c.Refused != "" {
				n += " (not usable: " + strings.SplitN(c.Refused, ",", 2)[0] + ")"
			} else if c.Exec != "" {
				n += " (runs " + c.Exec + ")"
			}
			names = append(names, ui.VisibleLine(n))
		}
		how := "each asked before its first use"
		if cfg.K8s.Enabled {
			how = "a credential helper asked before it first runs"
		}
		out = append(out, fmt.Sprintf("kubeconfig  %d contexts, %s", len(list), how))
		for _, n := range names {
			out = append(out, "            "+n)
		}
		if off != "" {
			out = append(out, "            a pasted login is refused here: "+off)
		}
	case off != "":
		out = append(out, "kubeconfig  not used, and a pasted login is refused: "+off)
	default:
		out = append(out, "kubeconfig  none; k8s_login is offered, so a pasted `oc login` can log in")
	}
	if c, ok := toolset.MachineSSH(cfg, true); ok {
		aliases := c.Aliases()
		out = append(out, fmt.Sprintf("ssh config  %d hosts, each asked before its first use: %s", len(aliases),
			ui.VisibleLine(strings.Join(aliases, ", "))))
		for _, u := range c.Unsupported {
			out = append(out, "            not read: "+ui.VisibleLine(u))
		}
	}
	gs, err := machinegrant.List()
	switch {
	case err != nil:
		out = append(out, "kept        UNREADABLE — "+err.Error())
	case len(gs) > 0:
		out = append(out, fmt.Sprintf("kept        %d (abhed machine list, abhed machine forget)", len(gs)))
		for _, g := range gs {
			out = append(out, "            "+g.Kind+" "+ui.VisibleLine(g.Name)+": "+ui.VisibleLine(g.What))
		}
	}
	return out
}

// k8sDoctor is what doctor says of the configured Kubernetes access. Where
// the organisation sets k8s it follows the same rule the tools do: its own
// k8s.enabled, and no kubeconfig read unless it names one. Servers are shown
// without any user, query or fragment.
func k8sDoctor(cfg config.Config) []string {
	enabled, noKubeconfig := toolset.OrgK8s(cfg)
	if !enabled {
		return nil
	}
	writes := "read-only"
	switch {
	case (cfg.ManagedSets("k8s.allow_writes") || cfg.FromOrg("k8s.allow_writes")) && !cfg.K8s.AllowWrites:
		writes = "read-only: writes off by your organisation"
	case cfg.K8s.AllowWrites:
		writes = "reads + writes (every write needs approval)"
	}
	out := []string{"kubernetes  " + writes}
	if noKubeconfig != "" {
		out = append(out, "            no kubeconfig read: "+noKubeconfig)
	} else if c, err := k8s.Open(k8s.Config{Kubeconfig: cfg.K8s.Kubeconfig,
		Context: cfg.K8s.Context, Namespace: cfg.K8s.Namespace,
		Token: os.Getenv("ABHED_K8S_TOKEN")}); err != nil {
		out = append(out, fmt.Sprintf("            UNAVAILABLE — %v", err))
	} else {
		out = append(out, fmt.Sprintf("            context %s\n            namespace %s · server %s",
			ui.VisibleLine(c.Name), ui.VisibleLine(c.Namespace), k8s.DisplayURL(c.Server)))
		if c.Insecure() {
			out = append(out, "            ⚠ the kubeconfig skips TLS verification for this cluster")
		}
	}
	for _, lc := range toolset.LoginClusters(cfg) {
		warn := ""
		if lc.InsecureSkipTLSVerify {
			warn = "  ⚠ TLS verification disabled"
		}
		out = append(out, fmt.Sprintf("k8s login   %s → %s%s", ui.VisibleLine(lc.Name), k8s.DisplayURL(lc.Server), warn))
	}
	return out
}
