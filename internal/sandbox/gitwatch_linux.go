//go:build linux

package sandbox

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// gitWatch moves aside a commondir made in a git folder bubblewrap lets
// commands write, the moment it is made: no file there can be bound before
// it exists, and the person's own git (an editor's, run on its own) would
// follow one to configuration that runs programs on the host. What it moved
// is said, and the next command refused, by takePlanted.
type gitWatch struct {
	mu      sync.Mutex
	fd      int
	wds     map[int]string // watch -> git folder
	dirs    map[string]bool
	moved   []map[string]any
	stopped chan struct{}
	done    chan struct{}
}

// watch watches dirs, git folders, adding any not yet watched; the watcher
// starts with the first.
func (gg *gitGuard) watch(dirs []string) {
	gg.mu.Lock()
	defer gg.mu.Unlock()
	if gg.closed {
		return
	}
	w := gg.w
	if w == nil {
		fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
		if err != nil {
			return
		}
		w = &gitWatch{fd: fd, wds: map[int]string{}, dirs: map[string]bool{}, stopped: make(chan struct{}), done: make(chan struct{})}
		gg.w = w
		go w.run()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, d := range dirs {
		if w.dirs[d] {
			continue
		}
		wd, err := unix.InotifyAddWatch(w.fd, d, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_ONLYDIR)
		if err != nil {
			continue
		}
		w.wds[wd], w.dirs[d] = d, true
		// One made before the watch began is taken out now.
		w.takeOut(filepath.Join(d, "commondir"))
	}
}

func (w *gitWatch) run() {
	defer close(w.done)
	buf := make([]byte, 64*1024)
	fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLIN}} // #nosec G115 -- a file descriptor
	for {
		select {
		case <-w.stopped:
			return
		default:
		}
		if _, err := unix.Poll(fds, 100); err != nil && !errors.Is(err, unix.EINTR) {
			return
		}
		n, err := unix.Read(w.fd, buf)
		if err != nil || n <= 0 {
			continue
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off])) // #nosec G103 -- reading the kernel's inotify record
			nameStart := off + unix.SizeofInotifyEvent
			nameEnd := nameStart + int(ev.Len)
			if nameEnd > n {
				break
			}
			name := strings.TrimRight(string(buf[nameStart:nameEnd]), "\x00")
			off = nameEnd
			if !strings.EqualFold(name, "commondir") {
				continue
			}
			w.mu.Lock()
			dir := w.wds[int(ev.Wd)]
			if dir != "" {
				w.takeOut(filepath.Join(dir, name))
			}
			w.mu.Unlock()
		}
	}
}

// takeOut moves aside p, a commondir in a repository's or submodule's git
// folder, where git never writes one; w.mu is held.
func (w *gitWatch) takeOut(p string) {
	if !exists(p) {
		return
	}
	entries, _ := takeOutGitPlanted([]string{p})
	w.moved = append(w.moved, entries...)
}

// takePlanted returns what the watcher moved aside since last asked.
func (gg *gitGuard) takePlanted() []map[string]any {
	gg.mu.Lock()
	w := gg.w
	gg.mu.Unlock()
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.moved
	w.moved = nil
	return out
}

// stop ends the watcher, for the sandbox closing.
func (gg *gitGuard) stop() {
	gg.mu.Lock()
	w := gg.w
	gg.w, gg.closed = nil, true
	gg.mu.Unlock()
	if w == nil {
		return
	}
	close(w.stopped)
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
	}
	_ = unix.Close(w.fd)
}
