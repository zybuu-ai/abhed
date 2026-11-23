package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// NetworkToolName is the tool that asks the person to open the network to
// commands where it is off.
const NetworkToolName = "network_access"

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

// Offer reports whether the person can be asked to open the network here,
// or why not.
func (n *Network) Offer() (bool, string) {
	if ok, why := NetworkOfferable(n.cfg); !ok {
		return false, why
	}
	if n.policy() == nil {
		return false, "this sandbox cannot hold the egress proxy commands would reach the network through (a container tier on a VM needs control_plane relay_binary)"
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

// State says what commands can reach now, in a line for the model.
func (n *Network) State() string {
	cfg := n.cfg
	switch {
	case sandboxconfig.NetworkOpen(cfg):
		return "on: commands can reach any host."
	case cfg.Sandbox.Network == config.NetworkAllowlist:
		pol := n.policy()
		if pol != nil && pol.AnyGranted() {
			return "on through the session's proxy: the person opened it to any host."
		}
		hosts := "the hosts the egress rules allow"
		if pol != nil {
			if h := pol.AllowedHosts(); len(h) > 0 {
				hosts = strings.Join(h, ", ")
			}
		}
		return "allowlist: commands reach only " + hosts + ", through the session's proxy."
	}
	pol := n.policy()
	if pol != nil && pol.AnyGranted() {
		return "on for http and https, which the user opened (ssh and ping still fail)."
	}
	if pol != nil {
		if h := pol.AllowedHosts(); len(h) > 0 {
			return "off, except hosts the user opened (http, https): " + strings.Join(h, ", ") + "."
		}
	}
	return "off: commands reach no host (curl, installs and git fetch, pull, push, clone fail)."
}

// NetworkTool asks the person, through the ordinary approval, to open the
// network to commands: for this workspace, one host, or this chat alone.
type NetworkTool struct{ N *Network }

func (NetworkTool) Name() string  { return NetworkToolName }
func (NetworkTool) Mutates() bool { return true }

func (NetworkTool) Description() string {
	return "Ask the user to open the network to the session's commands, where it is off. Call it when a command failed because " +
		"the network is off, or before one that needs it, with a reason the user can judge. scope is \"workspace\" (every later " +
		"session in this workspace too), \"host\" (only host, kept for this workspace) or \"once\" (this chat alone; host if given, " +
		"else any host). The user is asked first; if they refuse, say what cannot be done without it and stop. Never claim the network works when it is off."
}

func (NetworkTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "scope":{"type":"string","enum":["workspace","host","once"],"description":"workspace: any host, kept for this workspace; host: only host, kept; once: this chat alone."},
    "host":{"type":"string","description":"The one host, such as github.com: required for host, optional for once."},
    "reason":{"type":"string","description":"Why the task needs it, shown to the user."}
  },
  "required":["scope","reason"]
}`)
}

type networkArgs struct {
	Scope  string `json:"scope"`
	Host   string `json:"host"`
	Reason string `json:"reason"`
}

func (t NetworkTool) Run(_ context.Context, _ *tools.Session, raw json.RawMessage) tools.Result {
	var a networkArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tools.Result{Content: "network_access: " + err.Error(), IsError: true}
	}
	msg, err := t.N.Open(a.Scope, a.Host, true)
	if err != nil {
		return tools.Result{Content: "network_access: " + err.Error(), IsError: true}
	}
	return tools.Result{Content: "The user opened " + msg + ". Run the command again."}
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
	b.WriteString("[Session facts from Abhed. Do not test them by trial or dispute them with the user; if the user reports otherwise, believe what they saw.]\n")
	where := ""
	switch s.Tier() {
	case sandbox.TierContainer, sandbox.TierVM:
		where = ", image " + sandbox.SandboxImage()
	case sandbox.TierNone:
		where = ", no isolation"
	}
	fmt.Fprintf(&b, "- Sandbox: %s tier%s.\n", s.Tier(), where)
	fmt.Fprintf(&b, "- Network: %s", n.State())
	if ok, why := n.Offer(); ok && !sandboxconfig.NetworkOpen(cfg) {
		b.WriteString(" If needed, call " + NetworkToolName + "; the user is asked.")
	} else if !sandboxconfig.NetworkOpen(cfg) {
		b.WriteString(" It cannot be opened here: " + why + ".")
	}
	b.WriteString("\n- Git: runs in the sandbox; remote operations (fetch, pull, push, clone) are not brokered and need the network.\n")
	if missing, known := sandbox.MissingTools(s); known && len(missing) > 0 {
		fmt.Fprintf(&b, "- Not in the image: %s. Tell the user rather than trying them (/config set sandbox.tier, or sandbox.image).\n", strings.Join(missing, ", "))
	}
	var clusters []string
	for _, c := range cfg.K8s.Clusters {
		clusters = append(clusters, c.Name)
	}
	fmt.Fprintf(&b, "- Kubernetes clusters: %s.\n", orNone(strings.Join(clusters, ", ")))
	return strings.TrimRight(b.String(), "\n")
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
		b.NetworkNote = strings.TrimSpace(b.NetworkNote + " If the task needs the network, call " + NetworkToolName +
			" with a reason: the user is asked to open it for this workspace, one host, or this chat.")
	}
	return b
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
