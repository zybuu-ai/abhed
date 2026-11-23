package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

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

// machineIdentity sums what a grant for mh covers: where it goes, as whom,
// with which keys, and the host keys known_hosts holds for it, for the host
// and each jump host on the way. A changed HostName, User, Port, key,
// known_hosts entry or jump host asks again.
func machineIdentity(c *SSHConfig, mh MachineHost) string {
	parts := []string{mh.Alias}
	chain, err := hops(c, mh, nil)
	if err != nil {
		parts = append(parts, "hops:"+err.Error())
	}
	for _, h := range append([]MachineHost{mh}, chain...) {
		parts = append(parts, h.HostName, strconv.Itoa(h.Port), h.User,
			strings.Join(h.IdentityFiles, "\x00"), strings.Join(h.KnownHostsFiles, "\x00"),
			strings.Join(knownKeys(h), "\x00"))
	}
	return machinegrant.Identity(parts...)
}

// knownKeys are the fingerprints of the keys known_hosts holds for h.
func knownKeys(h MachineHost) []string {
	files := h.KnownHostsFiles
	if len(files) == 0 {
		files = []string{"~/.ssh/known_hosts", "~/.ssh/known_hosts2"}
	}
	var existing []string
	for _, f := range files {
		if _, err := os.Stat(expandHome(f)); err == nil {
			existing = append(existing, expandHome(f))
		}
	}
	probe := probeKey()
	if len(existing) == 0 || probe == nil {
		return nil
	}
	cb, err := knownhosts.New(existing...)
	if err != nil {
		return []string{"unreadable"}
	}
	var keyErr *knownhosts.KeyError
	if err := cb(h.Addr(), &net.TCPAddr{IP: net.IPv4zero, Port: h.Port}, probe); !errors.As(err, &keyErr) {
		return nil
	}
	var out []string
	for _, k := range keyErr.Want {
		out = append(out, ssh.FingerprintSHA256(k.Key))
	}
	sort.Strings(out)
	return out
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
	id := machineIdentity(r.machineConfig(), mh)
	if machinegrant.Kept(machinegrant.KindSSHHost, mh.Alias, id) {
		return tools.FirstUse{}, false
	}
	why := fmt.Sprintf("first use of %s from your ~/.ssh/config (%s@%s", mh.Alias, mh.User, mh.Addr())
	if mh.ProxyJump != "" {
		why += " through " + mh.ProxyJump
	}
	why += "), signing in with your ssh-agent or the key it names; the key never reaches the agent or the sandbox, " +
		"and the host's key must already be in known_hosts"
	scope := tools.FirstUseScope(machinegrant.KindSSHHost, mh.Alias)
	if machinegrant.Guest() {
		scope = "" // a guest keeps nothing: no "always" is offered
		why += "; as a guest, approving holds for this session only"
	}
	return tools.FirstUse{
		Scope:  scope,
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
