package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/policy"
)

// A subagent's worktree taken back by hunk (studio-acp-contract.md §7.6):
// the list shows each worktree's changes against the commit it shares with
// the session's workspace, and a take puts the accepted hunks in as the
// person's own writes through the manual path, as /take and the workbench do.

func init() {
	liveFeatures = append(liveFeatures, "worktrees")
	handle(map[string]func(*acpConn, rpcMessage){
		"_abhed/worktrees/list": (*acpConn).worktreesList,
		"_abhed/worktrees/take": (*acpConn).worktreesTake,
	})
}

func (c *acpConn) worktreesList(msg rpcMessage) {
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
	ctx := c.root()
	ws := parts.Session.Root
	out := []agent.WorktreeChanges{}
	list, err := agent.ListWorktrees(ctx, ws)
	if err != nil {
		// Not a repository, or no git: there is nothing to take.
		c.reply(msg.ID, map[string]any{"worktrees": out}, nil)
		return
	}
	for _, w := range list {
		ch, err := w.Changes(ctx, ws)
		if err != nil {
			continue
		}
		for i, f := range ch.Files {
			// What a read rule withholds is named, not shown, in the worktree as in the workspace.
			if readDenied(parts.Loop.Policy, filepath.Join(ws, filepath.FromSlash(f.Path))) || readDenied(parts.Loop.Policy, filepath.Join(w.Dir, filepath.FromSlash(f.Path))) {
				ch.Files[i].Hunks, ch.Files[i].Unsupported = nil, "denied by policy"
				continue
			}
			for j := range f.Hunks {
				h := &ch.Files[i].Hunks[j]
				for _, lines := range [][]string{h.Old, h.New, h.Before, h.After} {
					for k := range lines {
						lines[k] = redacted(lines[k])
					}
				}
			}
		}
		out = append(out, ch)
	}
	c.reply(msg.ID, map[string]any{"worktrees": out}, nil)
}

func (c *acpConn) worktreesTake(msg rpcMessage) {
	var p struct {
		Worktree string           `json:"worktree"`
		Accept   map[string][]int `json:"accept"`
		Remove   bool             `json:"remove"`
	}
	if err := json.Unmarshal(msg.Params, &p); err != nil || p.Worktree == "" {
		c.reply(msg.ID, nil, refusal(errParams, "worktree is required"))
		return
	}
	s, e := c.sessionFor(msg.Params)
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	parts, e := innerOf(s)
	if e == nil {
		e = idle(s)
	}
	if e != nil {
		c.reply(msg.ID, nil, e)
		return
	}
	ctx := c.root()
	ws := parts.Session.Root
	w, err := agent.FindWorktree(ctx, ws, p.Worktree)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errParams, "%v", err))
		return
	}
	tw := editorGuard{inner: parts.Loop.ManualTaker(parts.Session), s: s, ws: ws}
	res, err := parts.Loop.Take(ctx, ws, w, p.Accept, p.Remove, tw)
	if err != nil {
		c.reply(msg.ID, nil, refusal(errRefused, "%v", err))
		return
	}
	if len(res.Taken) > 0 {
		paths := make([]string, len(res.Taken))
		for i, rel := range res.Taken {
			paths[i] = filepath.Join(ws, filepath.FromSlash(rel))
		}
		c.notification("_abhed/review/changed", map[string]any{"sessionId": s.id, "paths": paths})
	}
	c.reply(msg.ID, res, nil)
}

// readDenied reports whether a read rule keeps path from the agent, and so
// from the person's view of it here.
func readDenied(pol *policy.Engine, path string) bool {
	if pol == nil {
		return false
	}
	args, _ := json.Marshal(map[string]string{"path": path})
	return pol.Evaluate("read", false, args).Decision == policy.Deny
}

// editorGuard keeps the editor's own files out of a take, as a review's
// reject keeps them.
type editorGuard struct {
	inner agent.TakeWriter
	s     *acpSession
	ws    string
}

func (g editorGuard) guard(rel string) error {
	if editorFile(filepath.Join(g.ws, filepath.FromSlash(rel)), g.s.roots(), g.s.protected) {
		return errors.New(errEditorFileUndo.Error())
	}
	return nil
}

func (g editorGuard) ReadFile(rel string) ([]byte, bool, error) { return g.inner.ReadFile(rel) }

func (g editorGuard) WriteFile(ctx context.Context, rel string, content []byte) error {
	if err := g.guard(rel); err != nil {
		return err
	}
	return g.inner.WriteFile(ctx, rel, content)
}

func (g editorGuard) RemoveFile(ctx context.Context, rel string) error {
	if err := g.guard(rel); err != nil {
		return err
	}
	return g.inner.RemoveFile(ctx, rel)
}
