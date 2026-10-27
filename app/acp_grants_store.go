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
	"github.com/zybuu-ai/abhed/internal/filelock"
	"github.com/zybuu-ai/abhed/internal/foldkey"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A bash "always" grant is kept per workspace in Abhed's own state, which
// no command can read or write, so a new session in that workspace starts
// with it; the person lists and revokes them (_abhed/grants/list, revoke).
// A grant for the session alone is never kept. Where the managed
// configuration holds the permissions none is loaded or offered. Every
// Abhed process shares the file: changes hold its lock, and a kept grant
// is looked up again before it answers, so a revoke anywhere holds.

// grantsFile keeps the workspaces' bash grants; tests set it.
var grantsFile = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".abhed", "state", "workspace-grants.json")
}

// grantsMu orders this process's changes; the file's lock orders processes'.
var grantsMu sync.Mutex

// keptGrant is one kept grant with when it was chosen.
type keptGrant struct {
	bashGrant
	GrantedAt time.Time `json:"granted_at"`
}

// keptWorkspace is one workspace's grants. Folder is the workspace folder's
// identity when they were kept: a folder deleted and made again at the same
// path is another, and starts with none.
type keptWorkspace struct {
	Folder string      `json:"folder,omitempty"`
	Grants []keptGrant `json:"grants"`
	// ListenAgreedAt is when the person let commands listen on loopback here.
	ListenAgreedAt *time.Time `json:"listen_agreed_at,omitempty"`
}

type keptGrants struct {
	Workspaces map[string]*keptWorkspace `json:"workspaces"`
}

// grantsKey names a workspace in the file: its real path as the disk spells
// it, case-folded where the disk ignores case, so every spelling is one.
func grantsKey(workspace string) string {
	p := tools.DiskPath(debugWorkspaceKey(workspace))
	if tools.FoldsCaseAt(p) {
		return foldkey.Key(p)
	}
	return p
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
		k.Workspaces = map[string]*keptWorkspace{}
	}
	return k, bad
}

// changeKeptGrants applies change to the file under its lock, across
// processes, and writes it back when change says so.
func changeKeptGrants(change func(*keptGrants) bool) error {
	f := grantsFile()
	if f == "" {
		return errors.New("there is no home folder to keep the grant in")
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	grantsMu.Lock()
	defer grantsMu.Unlock()
	unlock, err := filelock.Lock(f+".lock", 10*time.Second, "%s is held by another Abhed process (waited %s)")
	if err != nil {
		return err
	}
	defer unlock()
	k, bad := readKeptGrants()
	if !change(&k) {
		return nil
	}
	if bad {
		aside := f + ".unreadable-" + time.Now().UTC().Format("20060102T150405")
		if err := os.Rename(f, aside); err != nil {
			return err
		}
		warnf("the unreadable %s was moved to %s", f, aside)
	}
	b, _ := json.MarshalIndent(k, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(f), ".workspace-grants-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone already after the rename
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f)
}

// keptFor is what is kept for workspace, nil where the folder there is not
// the one it was kept for.
func keptFor(workspace string) *keptWorkspace {
	k, _ := readKeptGrants()
	w := k.Workspaces[grantsKey(workspace)]
	if w == nil || w.Folder != "" && w.Folder != folderID(workspace) {
		return nil
	}
	return w
}

// workspaceGrants are the grants kept for workspace.
func workspaceGrants(workspace string) []keptGrant {
	if w := keptFor(workspace); w != nil {
		return w.Grants
	}
	return nil
}

// listenAgreed is when the person let commands listen on loopback in
// workspace, or nil.
func listenAgreed(workspace string) *time.Time {
	if w := keptFor(workspace); w != nil {
		return w.ListenAgreedAt
	}
	return nil
}

// setListenAgreed keeps, or with on false forgets, that agreement.
func setListenAgreed(workspace string, on bool) error {
	key, folder := grantsKey(workspace), folderID(workspace)
	return changeKeptGrants(func(k *keptGrants) bool {
		w := k.Workspaces[key]
		if w == nil || w.Folder != folder {
			if !on {
				return false
			}
			w = &keptWorkspace{Folder: folder}
			k.Workspaces[key] = w
		}
		if on {
			now := time.Now().UTC()
			w.ListenAgreedAt = &now
		} else {
			w.ListenAgreedAt = nil
			if len(w.Grants) == 0 {
				delete(k.Workspaces, key)
			}
		}
		return true
	})
}

// stillKept reports whether g is kept for workspace now, as another
// process's revoke may have taken it away.
func stillKept(workspace string, g bashGrant) bool {
	return slices.ContainsFunc(workspaceGrants(workspace), func(have keptGrant) bool { return have.bashGrant == g })
}

// keepGrant keeps g for workspace, once.
func keepGrant(workspace string, g bashGrant) error {
	if g.Session {
		return nil
	}
	key, folder := grantsKey(workspace), folderID(workspace)
	return changeKeptGrants(func(k *keptGrants) bool {
		w := k.Workspaces[key]
		if w == nil || w.Folder != folder {
			// A folder made again at the path keeps nothing of the old one's.
			w = &keptWorkspace{Folder: folder}
			k.Workspaces[key] = w
		}
		if slices.ContainsFunc(w.Grants, func(have keptGrant) bool { return have.bashGrant == g }) {
			return false
		}
		w.Grants = append(w.Grants, keptGrant{bashGrant: g, GrantedAt: time.Now().UTC()})
		return true
	})
}

// dropGrant forgets the grant id names for workspace, and says which it was.
func dropGrant(workspace, id string) (bashGrant, bool, error) {
	key := grantsKey(workspace)
	var gone bashGrant
	found := false
	err := changeKeptGrants(func(k *keptGrants) bool {
		w := k.Workspaces[key]
		if w == nil {
			return false
		}
		for i, g := range w.Grants {
			if g.id() == id {
				gone, found = g.bashGrant, true
				w.Grants = append(w.Grants[:i:i], w.Grants[i+1:]...)
				if len(w.Grants) == 0 && w.ListenAgreedAt == nil {
					delete(k.Workspaces, key)
				}
				return true
			}
		}
		return false
	})
	return gone, found && err == nil, err
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

// openSessions are the connection's sessions now.
func (c *acpConn) openSessions() []*acpSession {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	open := make([]*acpSession, 0, len(c.sessions))
	for _, other := range c.sessions {
		open = append(open, other)
	}
	return open
}

// grantFor is the grant that covers command in the session's folder. One
// kept for the workspace is looked up again first: revoked in another
// process, it is dropped here and answers nothing.
func (s *acpSession) grantFor(command string) (bashGrant, bool) {
	cwd := s.workDir()
	for {
		s.mu.Lock()
		var hit bashGrant
		found := false
		for _, g := range s.grants {
			if g.covers(command, cwd) {
				hit, found = g, true
				break
			}
		}
		s.mu.Unlock()
		if !found || hit.Session || s.parts.Session == nil || stillKept(s.parts.Session.Root, hit) {
			return hit, found
		}
		s.mu.Lock()
		s.grants = slices.DeleteFunc(s.grants, func(have bashGrant) bool { return have == hit })
		s.mu.Unlock()
	}
}

// loadKeptGrants gives a new or continued session its workspace's kept
// grants, where the organisation leaves allow rules to people, each
// recorded with when it was chosen, so an answer one gives has its source.
func (s *acpSession) loadKeptGrants() {
	if !s.inner || s.parts.Config.AllowLocked() || s.parts.Session == nil {
		return
	}
	for _, g := range workspaceGrants(s.parts.Session.Root) {
		s.addGrant(g.bashGrant)
		ev := g.grantRecord()
		at := g.GrantedAt
		ev.Kept, ev.GrantedAt = "workspace", &at
		if _, err := s.parts.Loop.Recorder.Record(agent.EvApprovalScopeGranted, agent.ActorSystem, agent.Trusted, ev); err != nil {
			warnf("the record did not take %s: %v", agent.EvApprovalScopeGranted, err)
		}
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
	// Gone from this process's open sessions of the workspace at once; one in
	// another process finds it gone before the grant would next answer.
	root := grantsKey(parts.Session.Root)
	for _, other := range c.openSessions() {
		if !other.inner || other.parts.Session == nil || grantsKey(other.parts.Session.Root) != root {
			continue
		}
		other.mu.Lock()
		other.grants = slices.DeleteFunc(other.grants, func(have bashGrant) bool { return have == g })
		other.mu.Unlock()
	}
	s.record(agent.EvPermissionChanged, agent.PermissionChanged{Op: "remove", List: "allow", Rule: g.scopeOf(), Scope: "workspace", By: agent.ByUser})
	c.reply(msg.ID, map[string]any{"revoked": p.ID, "scope": g.scopeOf()}, nil)
}
