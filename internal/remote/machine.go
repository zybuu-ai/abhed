package remote

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A host from the person's ~/.ssh/config is reached with their own setup:
// its HostName, User, Port and ProxyJump, their ssh-agent and the keys it
// names, all in Abhed's process, never the sandbox's. Its key is checked
// against known_hosts, which Abhed never writes. The first use of each host
// asks once; "always" keeps it in the person's own state.

// machineTarget describes a ~/.ssh/config host for the approval.
func machineTarget(mh MachineHost) string {
	if mh.Refused != "" {
		return "host " + mh.Alias + " from your ~/.ssh/config, which Abhed will not use: " + mh.Refused
	}
	t := fmt.Sprintf("runs as %s@%s, from your ~/.ssh/config, signing in with your ssh-agent or its IdentityFile",
		mh.User, mh.Addr())
	if mh.ProxyJump != "" {
		t += ", through " + mh.ProxyJump
	}
	return t
}

// machineIdentity sums what a grant for mh covers: where it goes and as
// whom, so a changed HostName, User, Port, ProxyJump or key asks again.
func machineIdentity(mh MachineHost) string {
	return machinegrant.Identity(mh.Alias, mh.HostName, strconv.Itoa(mh.Port), mh.User, mh.ProxyJump,
		strings.Join(mh.IdentityFiles, "\x00"), strings.Join(mh.KnownHostsFiles, "\x00"))
}

// machineFirstUse is what a call naming host needs: nothing for a declared
// or session host, and the person's yes, once, for a ~/.ssh/config host.
func machineFirstUse(r *Registry, sess *tools.Session, host string) (tools.FirstUse, bool) {
	if r == nil || host == "" {
		return tools.FirstUse{}, false
	}
	if h, ok := sessionHostsOf(sess, r, false).get(host); ok && !h.cfg.machine {
		return tools.FirstUse{}, false
	}
	mh, ok := r.MachineHost(host)
	if !ok || mh.Refused != "" {
		return tools.FirstUse{}, false
	}
	id := machineIdentity(mh)
	if machinegrant.Kept(machinegrant.KindSSHHost, mh.Alias, id) {
		return tools.FirstUse{}, false
	}
	why := fmt.Sprintf("first use of %s from your ~/.ssh/config (%s@%s", mh.Alias, mh.User, mh.Addr())
	if mh.ProxyJump != "" {
		why += " through " + mh.ProxyJump
	}
	why += "), signing in with your ssh-agent or the key it names; the key never reaches the agent or the sandbox, " +
		"and the host's key must already be in known_hosts"
	return tools.FirstUse{
		Scope:  tools.FirstUseScope(machinegrant.KindSSHHost, mh.Alias),
		What:   "host " + mh.Alias + " (" + mh.User + "@" + mh.Addr() + ")",
		Reason: why, Kind: machinegrant.KindSSHHost, Name: mh.Alias, Identity: id,
	}, true
}

func keepMachine(g tools.FirstUse) error {
	return machinegrant.Keep(machinegrant.Grant{Kind: g.Kind, Name: g.Name, Identity: g.Identity, What: g.What})
}

func hostArg(raw json.RawMessage) string {
	var a struct {
		Host string `json:"host"`
	}
	_ = json.Unmarshal(raw, &a)
	return strings.TrimSpace(a.Host)
}

// FirstUse makes the first command on a ~/.ssh/config host say so.
func (t Tool) FirstUse(sess *tools.Session, raw json.RawMessage) (tools.FirstUse, bool) {
	return machineFirstUse(t.R, sess, hostArg(raw))
}

// KeepFirstUse keeps the host for the person.
func (Tool) KeepFirstUse(g tools.FirstUse) error { return keepMachine(g) }

// FirstUse makes the first transfer to a ~/.ssh/config host say so.
func (t PutTool) FirstUse(sess *tools.Session, raw json.RawMessage) (tools.FirstUse, bool) {
	return machineFirstUse(t.R, sess, hostArg(raw))
}

// KeepFirstUse keeps the host for the person.
func (PutTool) KeepFirstUse(g tools.FirstUse) error { return keepMachine(g) }

// FirstUse makes the first transfer from a ~/.ssh/config host say so.
func (t GetTool) FirstUse(sess *tools.Session, raw json.RawMessage) (tools.FirstUse, bool) {
	return machineFirstUse(t.R, sess, hostArg(raw))
}

// KeepFirstUse keeps the host for the person.
func (GetTool) KeepFirstUse(g tools.FirstUse) error { return keepMachine(g) }

// machineHostsLine names the ~/.ssh/config hosts for the descriptions.
func machineHostsLine(r *Registry) string {
	aliases := r.machineAliases()
	if len(aliases) == 0 {
		return ""
	}
	if len(aliases) > 30 {
		aliases = append(aliases[:30], fmt.Sprintf("and %d more", len(aliases)-30))
	}
	return " Hosts from the user's ~/.ssh/config, ready by name with their own ssh-agent and keys " +
		"(the first use of each asks the user once): " + strings.Join(aliases, ", ") + "."
}
