package cgroup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrUnsupported is returned where there are no cgroups: on every system but
// Linux.
var ErrUnsupported = errors.New("cgroup: cgroup v2 is only on Linux")

// ErrNotDelegated is wrapped when no cgroup this user may manage was found,
// or the one found lacks a controller the limits need.
var ErrNotDelegated = errors.New("cgroup: no delegated cgroup v2 subtree")

// howToDelegate ends every ErrNotDelegated: the way to get a subtree.
const howToDelegate = "run Abhed in a delegated cgroup, for example under " +
	"`systemd-run --user --scope -p Delegate=yes abhed ...`, or in a container whose cgroup is delegated to it"

func notDelegated(dir, format string, a ...any) error {
	return fmt.Errorf("%w at %s: %s; %s", ErrNotDelegated, dir, fmt.Sprintf(format, a...), howToDelegate)
}

// Names below the delegated cgroup.
const (
	hostLeaf      = "abhed-host"
	sandboxPrefix = "abhed-sandbox-"
	leafPrefix    = "call-"
)

// baseControllers are enabled for every sandbox: pids and memory bound a
// fork bomb and a memory hog, and their events are what the record shows.
var baseControllers = []string{"memory", "pids"}

// Base is the delegated cgroup that Abhed's sandboxes are made in.
type Base struct {
	dir string
	mu  sync.Mutex
}

// Path is the delegated cgroup's directory.
func (b *Base) Path() string { return b.dir }

// NewSandbox makes the cgroup of sandbox id with limits l. It fails, and
// makes nothing, when a controller l needs is not delegated or a limit
// cannot be written.
func (b *Base) NewSandbox(id string, l Limits) (*Sandbox, error) {
	if !validName(id) {
		return nil, fmt.Errorf("cgroup: sandbox id %q: want letters, digits, '.', '_' or '-', at most 64", id)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	ctrls := slices.Clone(baseControllers)
	for _, c := range l.controllers() {
		if !slices.Contains(ctrls, c) {
			ctrls = append(ctrls, c)
		}
	}
	if err := b.enable(ctrls); err != nil {
		return nil, err
	}
	dir := filepath.Join(b.dir, sandboxPrefix+id)
	if err := os.Mkdir(dir, 0o755); err != nil { // #nosec G301 -- a cgroup directory; its mode is the kernel's to enforce
		return nil, fmt.Errorf("cgroup: sandbox %s: %w", id, err)
	}
	s := &Sandbox{node: node{dir}, ctrls: ctrls, leaves: map[string]*Leaf{}}
	err := writeFile(filepath.Join(dir, "cgroup.subtree_control"), plus(ctrls))
	if err == nil {
		err = apply(dir, l)
	}
	if err != nil {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("cgroup: sandbox %s: %w", id, err)
	}
	return s, nil
}

// enable turns on ctrls for b's children. Writing cgroup.subtree_control is
// refused while b itself holds processes, so they move to the host leaf
// first, as delegation requires.
func (b *Base) enable(ctrls []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	avail := words(readFile(filepath.Join(b.dir, "cgroup.controllers")))
	var missing []string
	for _, c := range ctrls {
		if !avail[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return notDelegated(b.dir, "controller %s not delegated (have %q)",
			strings.Join(missing, ", "), strings.TrimSpace(readFile(filepath.Join(b.dir, "cgroup.controllers"))))
	}
	have := words(readFile(filepath.Join(b.dir, "cgroup.subtree_control")))
	var need []string
	for _, c := range ctrls {
		if !have[c] {
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return nil
	}
	ctl := filepath.Join(b.dir, "cgroup.subtree_control")
	err := writeFile(ctl, plus(need))
	if !errors.Is(err, syscall.EBUSY) {
		return err
	}
	if err := b.evacuate(); err != nil {
		return err
	}
	return writeFile(ctl, plus(need))
}

// evacuate moves the processes in b into the host leaf. Each must be this
// user's: another user's process in the subtree means it is not Abhed's.
func (b *Base) evacuate() error {
	host := filepath.Join(b.dir, hostLeaf)
	if err := os.Mkdir(host, 0o755); err != nil && !errors.Is(err, fs.ErrExist) { // #nosec G301 -- a cgroup directory
		return fmt.Errorf("cgroup: %w", err)
	}
	uid := os.Getuid()
	for range 100 {
		ps := pids(readFile(filepath.Join(b.dir, "cgroup.procs")))
		if len(ps) == 0 {
			return nil
		}
		for _, p := range ps {
			if owner, ok := ownerOf(p); ok && owner != uid {
				return notDelegated(b.dir, "it holds process %d of user %d, which Abhed will not move", p, owner)
			}
			if err := writeFile(filepath.Join(host, "cgroup.procs"), fmt.Sprint(p)); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("cgroup: moving process %d to %s: %w", p, host, err)
			}
		}
	}
	return fmt.Errorf("cgroup: %s still holds processes after moving them out", b.dir)
}

// Sandbox is the cgroup that bounds every command of one sandbox.
type Sandbox struct {
	node
	ctrls   []string
	mu      sync.Mutex
	leaves  map[string]*Leaf
	removed bool
}

// NewLeaf makes the cgroup for tool call callID, with its own limits l
// inside the sandbox's; a zero l only identifies and accounts the call.
func (s *Sandbox) NewLeaf(callID string, l Limits) (*Leaf, error) {
	if !validName(callID) {
		return nil, fmt.Errorf("cgroup: call id %q: want letters, digits, '.', '_' or '-', at most 64", callID)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	for _, c := range l.controllers() {
		if !slices.Contains(s.ctrls, c) {
			return nil, fmt.Errorf("cgroup: call %s: controller %s is not enabled in the sandbox", callID, c)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return nil, fmt.Errorf("cgroup: call %s: the sandbox was removed", callID)
	}
	// The kernel cannot tell a scope delegated to Abhed from one systemd still
	// manages, and a manager that turns a controller off drops its limits.
	// Each call checks they still hold rather than run unbounded.
	if err := s.controllersHeld(); err != nil {
		return nil, fmt.Errorf("cgroup: call %s: %w", callID, err)
	}
	dir := filepath.Join(s.dir, leafPrefix+callID)
	if err := os.Mkdir(dir, 0o755); err != nil { // #nosec G301 -- a cgroup directory
		return nil, fmt.Errorf("cgroup: call %s: %w", callID, err)
	}
	err := apply(dir, l)
	var fd *os.File
	if err == nil {
		fd, err = os.Open(dir) // #nosec G304 -- the leaf just made
	}
	if err != nil {
		_ = os.Remove(dir)
		return nil, fmt.Errorf("cgroup: call %s: %w", callID, err)
	}
	leaf := &Leaf{node: node{dir}, s: s, id: callID, fd: fd}
	s.leaves[callID] = leaf
	return leaf, nil
}

// controllersHeld checks every controller the sandbox was made with is
// still in force on it.
func (s *Sandbox) controllersHeld() error {
	have := words(readFile(filepath.Join(s.dir, "cgroup.controllers")))
	for _, c := range s.ctrls {
		if !have[c] {
			return notDelegated(s.dir, "controller %s was turned off after the sandbox was made, so its limits no longer apply", c)
		}
	}
	return nil
}

// Remove kills every process left in the sandbox, then removes its leaves
// and itself.
func (s *Sandbox) Remove(ctx context.Context) error {
	s.mu.Lock()
	s.removed = true
	leaves := make([]*Leaf, 0, len(s.leaves))
	for _, l := range s.leaves {
		leaves = append(leaves, l)
	}
	s.mu.Unlock()
	if err := s.Kill(ctx); err != nil && !gone(err) {
		return err
	}
	var errs []error
	for _, l := range leaves {
		errs = append(errs, l.Remove(ctx))
	}
	// A leaf made by someone else, or left by a crash, holds no process now.
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if e.IsDir() {
			errs = append(errs, rmdir(ctx, filepath.Join(s.dir, e.Name())))
		}
	}
	errs = append(errs, rmdir(ctx, s.dir))
	return errors.Join(errs...)
}

// gone reports an error from a cgroup removed meanwhile: a missing file, or
// ENODEV from an interface file of a cgroup already deleted.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENODEV)
}

// Leaf is the cgroup of one tool call.
type Leaf struct {
	node
	s  *Sandbox
	id string

	mu sync.Mutex
	fd *os.File // the directory, for CLONE_INTO_CGROUP
}

// Remove kills every process left in the leaf and removes it.
func (l *Leaf) Remove(ctx context.Context) error {
	if err := l.Kill(ctx); err != nil && !gone(err) {
		return err
	}
	l.mu.Lock()
	if l.fd != nil {
		_ = l.fd.Close()
		l.fd = nil
	}
	l.mu.Unlock()
	l.s.mu.Lock()
	delete(l.s.leaves, l.id)
	l.s.mu.Unlock()
	return rmdir(ctx, l.dir)
}

// node is one cgroup directory and what can be done to all of it.
type node struct{ dir string }

// Path is the cgroup's directory.
func (n node) Path() string { return n.dir }

// Freeze stops every process in the cgroup and returns once all are stopped.
func (n node) Freeze(ctx context.Context) error { return n.freeze(ctx, true) }

// Thaw lets the processes Freeze stopped run again.
func (n node) Thaw(ctx context.Context) error { return n.freeze(ctx, false) }

func (n node) freeze(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	if err := writeFile(filepath.Join(n.dir, "cgroup.freeze"), v); err != nil {
		return fmt.Errorf("cgroup: freeze: %w", err)
	}
	return poll(ctx, func() bool { return keyed(readFile(filepath.Join(n.dir, "cgroup.events")))["frozen"] == boolNum(on) })
}

// useKillFile is false in tests of the path for kernels before cgroup.kill.
var useKillFile = true

// Kill ends every process in the cgroup and its descendants, and returns once
// none is left. Below Linux 5.14, which has no cgroup.kill, it freezes the
// cgroup, so nothing forks while it is swept, and signals each process.
func (n node) Kill(ctx context.Context) error {
	kill := filepath.Join(n.dir, "cgroup.kill")
	if _, err := os.Stat(kill); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cgroup: kill: %w", err)
	} else if err == nil && useKillFile {
		if err := writeFile(kill, "1"); err != nil {
			return fmt.Errorf("cgroup: kill: %w", err)
		}
		return poll(ctx, func() bool { return !n.populated() })
	}
	if _, err := os.Stat(n.dir); err != nil {
		return fmt.Errorf("cgroup: kill: %w", err)
	}
	freeze := filepath.Join(n.dir, "cgroup.freeze")
	if writeFile(freeze, "1") == nil {
		defer func() { _ = writeFile(freeze, "0") }()
	}
	return poll(ctx, func() bool {
		for _, p := range n.allPids() {
			_ = killPid(p)
		}
		return !n.populated()
	})
}

// Events are the cgroup's OOM and pids-limit counts, its descendants'
// included. A count whose controller is off reads as zero.
func (n node) Events() (Events, error) {
	mem, err1 := os.ReadFile(filepath.Join(n.dir, "memory.events"))
	pid, err2 := os.ReadFile(filepath.Join(n.dir, "pids.events"))
	for _, err := range []error{err1, err2} {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Events{}, fmt.Errorf("cgroup: events: %w", err)
		}
	}
	if _, err := os.Stat(n.dir); err != nil {
		return Events{}, fmt.Errorf("cgroup: events: %w", err)
	}
	return eventsFrom(string(mem), string(pid)), nil
}

func (n node) populated() bool {
	return keyed(readFile(filepath.Join(n.dir, "cgroup.events")))["populated"] != 0
}

// allPids are the processes in the cgroup and every cgroup below it.
func (n node) allPids() []int {
	var out []int
	_ = filepath.WalkDir(n.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			out = append(out, pids(readFile(filepath.Join(p, "cgroup.procs")))...)
		}
		return nil
	})
	return out
}

// apply writes l's limits to the cgroup at dir. A file that is missing means
// a controller or a kernel feature is missing, and the limit is not applied:
// that is an error, never a skip.
func apply(dir string, l Limits) error {
	for _, s := range l.settings() {
		if err := writeFile(filepath.Join(dir, s.file), s.value); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s is not available, so the limit %q cannot be applied: %w", s.file, s.value, err)
			}
			return fmt.Errorf("writing %q to %s: %w", s.value, s.file, err)
		}
	}
	return nil
}

// writeFile writes v to a cgroup interface file in one write, never creating it.
func writeFile(name, v string) error {
	f, err := os.OpenFile(name, os.O_WRONLY, 0) // #nosec G304 -- a cgroup interface file
	if err != nil {
		return err
	}
	_, err = f.WriteString(v)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// readFile is name's contents, or nothing when it cannot be read.
func readFile(name string) string {
	b, _ := os.ReadFile(name) // #nosec G304 -- a cgroup interface file
	return string(b)
}

// plus is ctrls as cgroup.subtree_control enables them.
func plus(ctrls []string) string {
	out := make([]string, len(ctrls))
	for i, c := range ctrls {
		out[i] = "+" + c
	}
	return strings.Join(out, " ")
}

func boolNum(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// waitLimit bounds a wait whose context has no deadline.
const waitLimit = 10 * time.Second

// poll calls done until it reports true or ctx ends.
func poll(ctx context.Context, done func() bool) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, waitLimit)
		defer cancel()
	}
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for !done() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("cgroup: waiting: %w", ctx.Err())
		case <-t.C:
		}
	}
	return nil
}

// rmdir removes an empty cgroup, waiting out the moment after a kill when the
// kernel still counts an exiting process. A cgroup already gone is fine.
func rmdir(ctx context.Context, dir string) error {
	var err error
	_ = poll(ctx, func() bool {
		err = os.Remove(dir)
		return err == nil || errors.Is(err, fs.ErrNotExist) || !errors.Is(err, syscall.EBUSY)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("cgroup: removing %s: %w", dir, err)
	}
	return nil
}

// Sandboxes are the ids of the sandboxes in b, made by this run or another.
func (b *Base) Sandboxes() []string {
	entries, _ := os.ReadDir(b.dir)
	var out []string
	for _, e := range entries {
		if id, ok := strings.CutPrefix(e.Name(), sandboxPrefix); ok && e.IsDir() && validName(id) {
			out = append(out, id)
		}
	}
	return out
}

// RemoveSandbox kills what is left in sandbox id and removes it with its
// leaves: one a run that ended without removing it left behind.
func (b *Base) RemoveSandbox(ctx context.Context, id string) error {
	if !validName(id) {
		return fmt.Errorf("cgroup: sandbox id %q: want letters, digits, '.', '_' or '-', at most 64", id)
	}
	s := &Sandbox{node: node{filepath.Join(b.dir, sandboxPrefix+id)}, leaves: map[string]*Leaf{}}
	return s.Remove(ctx)
}
