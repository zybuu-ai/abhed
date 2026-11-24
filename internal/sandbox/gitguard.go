package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// gitGuard narrows, and does not close, the window in which a commondir a
// command plants in a git folder reaches the person's own git on the host, on
// a tier that binds only the git files that exist (bubblewrap, the container
// and fence tiers): a watch moves one aside soon after it is made (Linux),
// sweeps take it out as each command ends and as the sandbox closes, and the
// next command is refused and says why. A command that plants in a loop still
// has its file read by host git runs between a plant and its move (about a
// third of them in a measured 60 s run), and a repository a command makes is
// live to host tools until the command ends. Seatbelt denies the names
// themselves and needs none of this.
type gitGuard struct {
	mu     sync.Mutex
	w      *gitWatch
	closed bool
	// movedMu guards moved, what the watch moved aside since last asked, and
	// scanned, the workspaces the guard was asked to look at (its sweeps'),
	// apart from mu, which the watch holds while it moves.
	movedMu sync.Mutex
	moved   []map[string]any
	scanned []string
}

// gitScanned, set in tests, runs as a command's scan of the git folders ends
// (watchFound), before its refuse: where the watch may move a commondir made
// meanwhile.
var gitScanned func()

// noteMoved records what the watch moved aside.
func (gg *gitGuard) noteMoved(entries []map[string]any) {
	if len(entries) == 0 {
		return
	}
	gg.movedMu.Lock()
	gg.moved = append(gg.moved, entries...)
	gg.movedMu.Unlock()
}

// takePlanted returns what the watch moved aside since last asked.
func (gg *gitGuard) takePlanted() []map[string]any {
	gg.movedMu.Lock()
	defer gg.movedMu.Unlock()
	out := gg.moved
	gg.moved = nil
	return out
}

// workspaces are the workspaces the guard looked at, for its sweeps.
func (gg *gitGuard) workspaces() []string {
	gg.movedMu.Lock()
	defer gg.movedMu.Unlock()
	return append([]string(nil), gg.scanned...)
}

// refuse is why this command may not run: the watch moved a planted
// commondir aside since the last command was built, including during this
// one's scan; recorded. Called after the scan, so what the watch moves while
// the scan looks is never missed by both.
func (gg *gitGuard) refuse(ctx context.Context, backend, workspace string, mem *gitMemory) error {
	moved := gg.takePlanted()
	if len(moved) == 0 {
		return nil
	}
	// One made again since is taken out now, not left for the watch.
	sweepGit([]string{workspace}, backend, mem)
	if l := LaunchOf(ctx); l.Record != nil {
		_ = l.Record(EvGitPlanted, map[string]any{"entries": moved, "call_id": l.CallID, "sandbox": backend,
			"workspace": workspace, "reason": "a commondir was made in a git folder while a command ran, and moved aside"})
	}
	return errors.New("sandbox: the command was not run: an earlier command made a commondir in a git folder, " +
		"pointing git at configuration and hooks elsewhere; it was moved aside as it was made")
}

// watchFound watches the git folders mem's last scan of ws found, and
// remembers ws for the sweeps.
func (gg *gitGuard) watchFound(mem *gitMemory, ws string) {
	mem.mu.Lock()
	dirs := mem.dirs[ws]
	mem.mu.Unlock()
	gg.movedMu.Lock()
	if !slices.Contains(gg.scanned, ws) {
		gg.scanned = append(gg.scanned, ws)
	}
	gg.movedMu.Unlock()
	gg.watch(dirs)
	if gitScanned != nil {
		gitScanned()
	}
}

// sweep takes out what a command left in the workspaces' git folders that git
// would follow (a commondir, a repository it made), as the next command's
// start would: the person's own git may run before any next command.
func sweepGit(workspaces []string, backend string, mem *gitMemory) {
	for _, ws := range workspaces {
		_, _ = gitGuardFor(context.Background(), ws, backend, mem, false)
	}
}
