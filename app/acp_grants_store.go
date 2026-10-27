package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// A bash "always" grant is kept per workspace in Abhed's own state, which
// no command can read or write, so a new session in that workspace starts
// with it; the person lists and revokes them (_abhed/grants/list, revoke).
// A grant for the session alone is never kept. Where the managed
// configuration holds the permissions none is loaded or offered.

// grantsFile keeps the workspaces' bash grants; tests set it.
var grantsFile = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".abhed", "state", "bash-grants.json")
}

var grantsMu sync.Mutex

// keptGrant is one kept grant with when it was chosen.
type keptGrant struct {
	bashGrant
	GrantedAt time.Time `json:"granted_at"`
}

type keptGrants struct {
	Workspaces map[string][]keptGrant `json:"workspaces"`
}

// readKeptGrants reads the file; one that cannot be read keeps nothing,
// and is set aside rather than overwritten when written next.
func readKeptGrants() (k keptGrants, bad bool) {
	if f := grantsFile(); f != "" {
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &k) != nil { // #nosec G304 -- Abhed's own state file
			warnf("%s is not a list of bash grants, so none is taken from it", f)
			bad = true
			k = keptGrants{}
		}
	}
	if k.Workspaces == nil {
		k.Workspaces = map[string][]keptGrant{}
	}
	return k, bad
}

func writeKeptGrants(k keptGrants, bad bool) error {
	f := grantsFile()
	if f == "" {
		return errors.New("there is no home folder to keep the grant in")
	}
	if bad {
		aside := f + ".unreadable-" + time.Now().UTC().Format("20060102T150405")
		if err := os.Rename(f, aside); err != nil {
			return err
		}
		warnf("the unreadable %s was moved to %s", f, aside)
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(k, "", "  ")
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}

// workspaceGrants are the grants kept for workspace.
func workspaceGrants(workspace string) []keptGrant {
	grantsMu.Lock()
	defer grantsMu.Unlock()
	k, _ := readKeptGrants()
	return k.Workspaces[debugWorkspaceKey(workspace)]
}

// keepGrant keeps g for workspace, once.
func keepGrant(workspace string, g bashGrant) error {
	if g.Session {
		return nil
	}
	grantsMu.Lock()
	defer grantsMu.Unlock()
	k, bad := readKeptGrants()
	key := debugWorkspaceKey(workspace)
	if slices.ContainsFunc(k.Workspaces[key], func(have keptGrant) bool { return have.bashGrant == g }) {
		return nil
	}
	k.Workspaces[key] = append(k.Workspaces[key], keptGrant{bashGrant: g, GrantedAt: time.Now().UTC()})
	return writeKeptGrants(k, bad)
}

// dropGrant forgets the grant id names for workspace, and says which it was.
func dropGrant(workspace, id string) (bashGrant, bool, error) {
	grantsMu.Lock()
	defer grantsMu.Unlock()
	k, bad := readKeptGrants()
	key := debugWorkspaceKey(workspace)
	list := k.Workspaces[key]
	for i, g := range list {
		if g.id() == id {
			k.Workspaces[key] = append(list[:i:i], list[i+1:]...)
			if len(k.Workspaces[key]) == 0 {
				delete(k.Workspaces, key)
			}
			return g.bashGrant, true, writeKeptGrants(k, bad)
		}
	}
	return bashGrant{}, false, nil
}

// id names a grant stably, for revoking it.
func (g bashGrant) id() string {
	sum := sha256.Sum256([]byte(g.Command + "\x00" + g.Family + "\x00" + g.Folder))
	return "g-" + hex.EncodeToString(sum[:6])
}

// addGrant puts g in s's grants, once.
func (s *acpSession) addGrant(g bashGrant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.grants, g) {
		s.grants = append(s.grants, g)
	}
}

// loadKeptGrants gives a new or continued session its workspace's kept
// grants, where the organisation leaves allow rules to people.
func (s *acpSession) loadKeptGrants() {
	if !s.inner || s.parts.Config.AllowLocked() || s.parts.Session == nil {
		return
	}
	for _, g := range workspaceGrants(s.parts.Session.Root) {
		s.addGrant(g.bashGrant)
	}
}

func init() {
	liveFeatures = append(liveFeatures, "grants")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/grants/list":   (*acpConn).grantsList,
		"_abhed/grants/revoke": (*acpConn).grantsRevoke,
	})
}

func (c *acpConn) grantsList(msg rpcMessage) {
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	out := []map[string]any{}
	for _, g := range workspaceGrants(parts.Session.Root) {
		item := map[string]any{"id": g.id(), "scope": g.scopeOf(), "grantedAt": g.GrantedAt}
		if g.Command != "" {
			item["command"] = g.Command
		}
		if g.Family != "" {
			item["family"] = g.Family
		}
		if g.Folder != "" {
			item["folder"] = g.Folder
		}
		out = append(out, item)
	}
	c.reply(msg.ID, map[string]any{"workspace": parts.Session.Root, "grants": out, "managed": parts.Config.AllowLocked()}, nil)
}

func (c *acpConn) grantsRevoke(msg rpcMessage) {
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(msg.Params, &p)
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	if why := inAgentCommand(); why != "" {
		c.reply(msg.ID, nil, refusal(errPolicy, "revoking a grant is refused inside an agent's command (%s)", why))
		return
	}
	g, ok, err := dropGrant(parts.Session.Root, p.ID)
	switch {
	case err != nil:
		c.reply(msg.ID, nil, refusal(errRefused, "the grant could not be revoked: %v", err))
		return
	case !ok:
		c.reply(msg.ID, nil, refusal(errParams, "no kept grant %q in this workspace", p.ID))
		return
	}
	// Gone from every open session of the workspace too.
	root := debugWorkspaceKey(parts.Session.Root)
	c.sessMu.Lock()
	open := make([]*acpSession, 0, len(c.sessions))
	for _, other := range c.sessions {
		open = append(open, other)
	}
	c.sessMu.Unlock()
	for _, other := range open {
		if !other.inner || other.parts.Session == nil || debugWorkspaceKey(other.parts.Session.Root) != root {
			continue
		}
		other.mu.Lock()
		other.grants = slices.DeleteFunc(other.grants, func(have bashGrant) bool { return have == g })
		other.mu.Unlock()
	}
	s.record(agent.EvPermissionChanged, agent.PermissionChanged{Op: "remove", List: "allow", Rule: g.scopeOf(), Scope: "workspace", By: agent.ByUser})
	c.reply(msg.ID, map[string]any{"revoked": p.ID, "scope": g.scopeOf()}, nil)
}
