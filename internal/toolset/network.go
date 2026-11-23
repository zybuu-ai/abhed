package toolset

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// NetworkOfferText ends what a command that failed for the network is told.
// Studio learns of such a failure from the observation's network_off, never
// from this text. The network is opened by the person alone (Studio, or
// /network at the terminal); the agent can only say it needs it.
const NetworkOfferText = "Tell the user what needs the network: they can open it for this workspace, one host or this chat."

// KeptNetwork is what a person opened to commands in a workspace, kept in
// their own state: the whole network, or some hosts.
type KeptNetwork struct {
	All   bool     `json:"all,omitempty"`
	Hosts []string `json:"hosts,omitempty"`
}

// NetworkOfferable reports whether a person may open the network to
// commands from a session, or why not: never where the managed file holds
// the network, and not where it is open already.
func NetworkOfferable(cfg config.Config) (bool, string) {
	switch {
	case cfg.ManagedSets("sandbox.allow_network") || cfg.ManagedSets("sandbox.network") || cfg.ManagedSets("egress"):
		return false, "the managed configuration holds the network; ask your administrator"
	case sandboxconfig.NetworkOpen(cfg):
		return false, "the network is open to commands already"
	}
	return true, ""
}

// Network is a session's network as the person may open it: the grants
// kept for the workspace, applied once commands first run, and the tool
// that asks for more.
type Network struct {
	cfg config.Config
	sb  sandbox.Sandbox
	// Kept is what the person opened to this workspace before; nil keeps nothing.
	Kept func() KeptNetwork
	// Keep keeps a grant for the workspace: all, or host; nil keeps none.
	Keep func(all bool, host string) error

	once    sync.Once
	mu      sync.Mutex
	session []string // hosts opened for this session alone
	allOnce bool     // the whole network opened for this session alone
}

// NewNetwork is the network of a session whose commands run in sb.
func NewNetwork(cfg config.Config, sb sandbox.Sandbox) *Network {
	return &Network{cfg: cfg, sb: sb}
}

// policy is the egress policy the session's commands go through, nil where
// the sandbox holds none.
func (n *Network) policy() *egress.Policy {
	if n == nil || n.sb == nil {
		return nil
	}
	return sandbox.EgressOf(resolved(n.sb))
}

// tier is the tier commands run under, "" before it is known.
func (n *Network) tier() sandbox.Tier {
	if n == nil || n.sb == nil {
		return ""
	}
	if s := resolved(n.sb); s != nil {
		return s.Tier()
	}
	return ""
}

// Offer reports whether the person can be asked to open the network here,
// or why not.
func (n *Network) Offer() (bool, string) {
	if n.tier() == sandbox.TierNone {
		return false, "commands run on the host with no sandbox, so they have its network already"
	}
	if ok, why := NetworkOfferable(n.cfg); !ok {
		return false, why
	}
	if n.policy() == nil {
		switch n.tier() {
		case sandbox.TierContainer, sandbox.TierVM:
			return false, "this container tier cannot hold the egress proxy commands would go through (a container engine on a VM needs control_plane relay_binary)"
		}
		return false, "this sandbox holds no egress proxy for commands"
	}
	return true, ""
}

// ApplyKept puts the workspace's kept grants in force, once, before the
// first command; where none may be offered, nothing kept is used.
func (n *Network) ApplyKept() {
	if n == nil {
		return
	}
	n.once.Do(func() {
		if n.Kept == nil {
			return
		}
		if ok, _ := n.Offer(); !ok {
			return
		}
		k := n.Kept()
		pol := n.policy()
		if k.All {
			pol.GrantAny("network opened for this workspace")
		}
		if len(k.Hosts) > 0 {
			_ = pol.GrantHosts("network opened for this workspace:", k.Hosts)
		}
	})
}

// maxFactHosts bounds the hosts the facts line names, so it stays short.
const maxFactHosts = 8

// hostList names at most maxFactHosts hosts, and how many more.
func hostList(h []string) string {
	if len(h) <= maxFactHosts {
		return strings.Join(h, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(h[:maxFactHosts], ", "), len(h)-maxFactHosts)
}

// State says what commands can reach now, in a line for the model.
func (n *Network) State() string {
	cfg := n.cfg
	switch {
	case n.tier() == sandbox.TierNone:
		return "on (no sandbox: the host's network)"
	case sandboxconfig.NetworkOpen(cfg):
		return "on"
	case cfg.Sandbox.Network == config.NetworkAllowlist:
		pol := n.policy()
		if pol != nil && pol.AnyGranted() {
			return "on (opened by the user, via proxy)"
		}
		hosts := "the hosts the egress rules allow"
		if pol != nil {
			if h := pol.AllowedHosts(); len(h) > 0 {
				hosts = hostList(h)
			}
		}
		return "allowlist only: " + hosts
	}
	pol := n.policy()
	if pol != nil && pol.AnyGranted() {
		return "http(s) only, opened by the user"
	}
	if pol != nil {
		if h := pol.AllowedHosts(); len(h) > 0 {
			return "off except " + hostList(h)
		}
	}
	return "off"
}

// Open opens the network to the session's commands on the person's word:
// scope workspace (any host, kept), host (one host, kept) or once (this
// chat alone; host when given, else any). keep false keeps nothing, for a
// grant another session of the workspace already kept. It says what opened.
func (n *Network) Open(scope, host string, keep bool) (string, error) {
	if ok, why := n.Offer(); !ok {
		return "", fmt.Errorf("the network cannot be opened from this session: %s; tell the user, and do not retry", why)
	}
	n.ApplyKept()
	if host != "" {
		h, err := egress.CanonicalHost(strings.ToLower(strings.TrimSpace(host)))
		if err != nil || strings.HasPrefix(h, "*") {
			return "", fmt.Errorf("%q is not one host name", host)
		}
		host = h
	}
	pol := n.policy()
	kept := func(all bool, h string) string {
		if !keep {
			return ""
		}
		return n.keep(all, h)
	}
	switch scope {
	case "workspace":
		pol.GrantAny("network opened for this workspace")
		return "the network to commands in this workspace (http and https through the session's proxy; ssh and ping still fail)" + kept(true, ""), nil
	case "host":
		if host == "" {
			return "", errors.New("scope host needs host")
		}
		if err := pol.GrantHosts("network opened for this workspace:", []string{host}); err != nil {
			return "", err
		}
		return host + " to commands (http and https)" + kept(false, host), nil
	case "once":
		n.mu.Lock()
		defer n.mu.Unlock()
		if host == "" {
			pol.GrantAny("network opened for this chat")
			n.allOnce = true
			return "the network to commands for this chat only (http and https)", nil
		}
		if err := pol.GrantHosts("network opened for this chat:", []string{host}); err != nil {
			return "", err
		}
		n.session = append(n.session, host)
		return host + " to commands for this chat only", nil
	}
	return "", errors.New("scope is workspace, host or once")
}

// keep keeps a grant, saying where it was not kept.
func (n *Network) keep(all bool, host string) string {
	if n.Keep == nil {
		return " for this chat (it could not be kept for the workspace)"
	}
	if err := n.Keep(all, host); err != nil {
		return " for this chat; it could not be kept for the workspace: " + err.Error()
	}
	return ", kept for later sessions here"
}

// SessionNote is the first message's statement of what this session can
// do, from its real state: the tier, the network, git, clusters, and the
// tools the image lacks. The model is told so it neither finds the limits
// by trial nor argues about them with the person.
func SessionNote(cfg config.Config, sb sandbox.Sandbox, n *Network) string {
	if sb == nil {
		return ""
	}
	s := resolved(sb)
	if s == nil {
		return ""
	}
	n.ApplyKept()
	var b strings.Builder
	// Kept short: it is in every request of the session.
	b.WriteString("True now; don't probe or dispute; trust what the user reports. ")
	where := ""
	switch s.Tier() {
	case sandbox.TierContainer, sandbox.TierVM:
		where = ", image " + sandbox.SandboxImage()
	case sandbox.TierNone:
		where = ", no isolation"
	}
	fmt.Fprintf(&b, "Sandbox: %s%s. Network: %s", s.Tier(), where, n.State())
	if ok, why := n.Offer(); ok && !sandboxconfig.NetworkOpen(cfg) {
		b.WriteString(", only the user can open it")
	} else if !sandboxconfig.NetworkOpen(cfg) && s.Tier() != sandbox.TierNone {
		b.WriteString("; cannot be opened: " + why)
	}
	switch off := sandboxconfig.HostGitOff(cfg); {
	case hostgit.BrokersRemotes(s) && off == "":
		b.WriteString(". Git fetch, pull, push and clone reach remotes through the user's own git on the host (their pre-push hook runs here).")
	case hostgit.BrokersRemotes(s):
		b.WriteString(". Git remotes not brokered: " + off + ".")
	default:
		b.WriteString(". Git remotes not brokered.")
	}
	if missing, known := sandbox.MissingTools(s); known && len(missing) > 0 {
		fmt.Fprintf(&b, " Not in image: %s.", strings.Join(missing, ", "))
	}
	// Clusters only where the session has the Kubernetes tools to use them.
	var clusters []string
	if cfg.K8s.Enabled {
		for _, c := range cfg.K8s.Clusters {
			clusters = append(clusters, c.Name)
		}
	}
	if len(clusters) > 0 {
		fmt.Fprintf(&b, " Clusters: %s.", strings.Join(clusters, ", "))
	}
	return plainFacts(strings.TrimRight(b.String(), "\n"))
}

// withNetwork puts the workspace's kept network grants in force before a
// command or a terminal first runs, and tells a command that could not reach
// the network how to ask for it.
func withNetwork(b tools.Bash, n *Network, offered bool) tools.Bash {
	if b.Sandbox != nil {
		inner := b.Sandbox
		b.Sandbox = func(ctx context.Context, cwd, line string) *exec.Cmd {
			n.ApplyKept()
			return inner(ctx, cwd, line)
		}
	}
	if b.Shell != nil {
		inner := b.Shell
		b.Shell = func(ctx context.Context, cwd string) *exec.Cmd {
			n.ApplyKept()
			return inner(ctx, cwd)
		}
	}
	if ok, _ := NetworkOfferable(n.cfg); ok && offered {
		b.NetworkNote = strings.TrimSpace(b.NetworkNote + " " + NetworkOfferText)
	}
	return b
}

// plainFacts keeps the facts line one line of printable text, whatever a
// name in it (an image, a cluster, a host) holds.
func plainFacts(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
}
