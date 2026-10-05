package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ShellOutputCap is how much of a background command's output is kept: the
// last 1 MiB, older output dropped with a marker when read.
const ShellOutputCap = 1 << 20

// ShellHost starts and keeps a session's background commands. The loop puts
// one in a tool call's context; without one, run_in_background is refused.
type ShellHost interface {
	StartShell(ctx context.Context, req ShellRequest) (id string, err error)
}

// ShellRequest is a background command that policy has allowed, ready to start.
type ShellRequest struct {
	Command     string
	Description string
	// Secrets are the names given to the command, never their values.
	Secrets []string
	// Timeout bounds the command's life; zero leaves it to the host's limit.
	Timeout time.Duration
	// Tier is the sandbox the command runs under, as Result.Tier names it.
	Tier string
	// RanUnder, when set, is read once Build has made the command, for a
	// sandbox chosen as its first command is built; it replaces Tier.
	RanUnder func() string
	// Build makes the command on ctx, whose end kills its process group.
	Build func(ctx context.Context) (*exec.Cmd, error)
	// Proc, in place of Build, is a command already running in the
	// foreground that the person moved to the background; the host ends it
	// with Proc.Stop. Started is when it started.
	Proc    *ShellProc
	Started time.Time
}

// Detach lets a person move a foreground command to the background while it
// runs: Move is closed to ask, and Running reports whether a command is
// running that can be moved.
type Detach struct {
	Move    chan struct{}
	once    sync.Once
	running atomic.Bool
}

// SetRunning says whether something that can be moved is running now; a
// tool that can be moved sets it while it runs.
func (d *Detach) SetRunning(on bool) {
	if d != nil {
		d.running.Store(on)
	}
}

// NewDetach is a Detach for one call.
func NewDetach() *Detach { return &Detach{Move: make(chan struct{})} }

// Ask moves the call's command to the background if one is running, and
// reports whether it did.
func (d *Detach) Ask() bool {
	if d == nil || !d.running.Load() {
		return false
	}
	d.once.Do(func() { close(d.Move) })
	return true
}

type detachKey struct{}

// WithDetach lets the call's foreground command be moved to the background.
func WithDetach(ctx context.Context, d *Detach) context.Context {
	return context.WithValue(ctx, detachKey{}, d)
}

// DetachOf is the call's Detach, or nil.
func DetachOf(ctx context.Context) *Detach {
	d, _ := ctx.Value(detachKey{}).(*Detach)
	return d
}

type shellHostKey struct{}

// WithShellHost gives a tool call the session's background command host.
func WithShellHost(ctx context.Context, h ShellHost) context.Context {
	return context.WithValue(ctx, shellHostKey{}, h)
}

// ShellHostOf is the host a call may start background commands on, or nil.
func ShellHostOf(ctx context.Context) ShellHost {
	h, _ := ctx.Value(shellHostKey{}).(ShellHost)
	return h
}

// ShellProc is one started background command and the output it has written.
type ShellProc struct {
	cmd     *exec.Cmd
	stop    func()
	out     *shellRing
	done    chan struct{}
	started time.Time

	mu     sync.Mutex
	cursor int64
	exit   int
	err    error
	ended  time.Time
}

// liveProcs are the background commands still running in this process, for
// EndBackgroundShells.
var liveProcs sync.Map // *ShellProc -> struct{}

// StartShellProc starts cmd with its output kept in a ring of capBytes (zero
// is ShellOutputCap). stop must end cmd, as cancelling its context does.
func StartShellProc(cmd *exec.Cmd, stop func(), capBytes int) (*ShellProc, error) {
	if capBytes <= 0 {
		capBytes = ShellOutputCap
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	p := &ShellProc{cmd: cmd, stop: stop, out: &shellRing{max: capBytes}, done: make(chan struct{}), started: time.Now()}
	cmd.Stdout, cmd.Stderr = w, w
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(p.out, r)
	}()
	err = cmd.Start()
	_ = w.Close() // the command has its own copy; this one would hold the pipe open
	if err != nil {
		_ = r.Close()
		<-copied
		return nil, err
	}
	liveProcs.Store(p, struct{}{})
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	go p.watch(waited, copied, r)
	return p, nil
}

// watch waits for the command's end and the last of its output, then marks
// the shell ended with its exit status.
func (p *ShellProc) watch(waited <-chan error, copied <-chan struct{}, r *os.File) {
	werr := <-waited
	// A process left behind may hold the pipe; read a little longer, as in the foreground.
	select {
	case <-copied:
	case <-time.After(bashOutputWait):
		_ = r.Close()
		<-copied
	}
	_ = r.Close()
	p.mu.Lock()
	p.ended = time.Now()
	var ee *exec.ExitError
	switch {
	case werr == nil:
	case errors.As(werr, &ee):
		p.exit = ExitStatus(ee)
	default:
		p.exit, p.err = -1, werr
	}
	p.mu.Unlock()
	liveProcs.Delete(p)
	close(p.done)
}

// Done is closed once the command has ended and its output is read.
func (p *ShellProc) Done() <-chan struct{} { return p.done }

// Pid is the process the command runs as, which leads its process group.
func (p *ShellProc) Pid() int { return p.cmd.Process.Pid }

// Stop ends the command and its process group.
func (p *ShellProc) Stop() { p.stop() }

// Exit reports the exit status once the command has ended, and how long it ran.
func (p *ShellProc) Exit() (code int, ended bool, ran time.Duration) {
	select {
	case <-p.done:
	default:
		return 0, false, time.Since(p.started)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit, true, p.ended.Sub(p.started)
}

// Size is how many bytes the command has written, and how many were dropped
// from the ring.
func (p *ShellProc) Size() (total, dropped int64) {
	return p.out.size()
}

// ShellRead is the output a read returned and what it left out.
type ShellRead struct {
	Text string
	// Dropped is output the ring no longer held when it was read.
	Dropped int64
	// Skipped is output left out to keep this read within its size.
	Skipped int64
}

// ReadNew returns the output written since the last ReadNew, at most max
// bytes of it: the latest, since the end of output is where errors are.
func (p *ShellProc) ReadNew(max int) ShellRead {
	p.mu.Lock()
	defer p.mu.Unlock()
	text, dropped, skipped, next := p.out.since(p.cursor, max)
	p.cursor = next
	return ShellRead{Text: text, Dropped: dropped, Skipped: skipped}
}

// Unread is how many bytes were written since the last ReadNew.
func (p *ShellProc) Unread() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	total, _ := p.out.size()
	return total - p.cursor
}

// LastLine is the last non-empty line of output, clipped to max runes.
func (p *ShellProc) LastLine(max int) string {
	tail := p.out.tail(4096)
	lines := strings.Split(strings.TrimRight(tail, "\r\n \t"), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])
	if r := []rune(line); len(r) > max {
		line = string(r[:max]) + "…"
	}
	return line
}

// EndBackgroundShells stops every background command this process started
// and waits up to wait for them, so none outlives an exit.
func EndBackgroundShells(wait time.Duration) {
	var procs []*ShellProc
	liveProcs.Range(func(k, _ any) bool {
		procs = append(procs, k.(*ShellProc))
		return true
	})
	for _, p := range procs {
		p.Stop()
	}
	deadline := time.After(wait)
	for _, p := range procs {
		select {
		case <-p.done:
		case <-deadline:
			return
		}
	}
}

// shellRing keeps the last max bytes written to it. It trims lazily, at twice
// max, so a stream of small writes does not copy the buffer each time.
type shellRing struct {
	mu    sync.Mutex
	max   int
	buf   []byte
	base  int64 // the offset of buf[0] in everything written
	total int64
}

func (r *shellRing) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, b...)
	r.total += int64(len(b))
	if len(r.buf) > 2*r.max {
		cut := len(r.buf) - r.max
		r.buf = append(r.buf[:0:0], r.buf[cut:]...)
		r.base += int64(cut)
	}
	return len(b), nil
}

// kept is the offset of the oldest byte still held. The caller holds mu.
func (r *shellRing) kept() int64 {
	return max(r.base, r.total-int64(r.max))
}

func (r *shellRing) size() (total, dropped int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total, r.kept()
}

// since returns the output from cursor on, at most limit bytes of its end, and
// the offset the next read starts at.
func (r *shellRing) since(cursor int64, limit int) (text string, dropped, skipped, next int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	from := cursor
	if k := r.kept(); from < k {
		dropped, from = k-from, k
	}
	if limit > 0 && r.total-from > int64(limit) {
		skipped = r.total - from - int64(limit)
		from += skipped
	}
	b := r.buf[from-r.base:]
	// Start on a whole character: a cut never splits one.
	for n := 0; n < utf8.UTFMax && len(b) > 0 && !utf8.RuneStart(b[0]); n++ {
		b = b[1:]
		skipped++
	}
	return strings.ToValidUTF8(string(b), "�"), dropped, skipped, r.total
}

// tail is the last n bytes held.
func (r *shellRing) tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	from := max(r.kept(), r.total-int64(n))
	return strings.ToValidUTF8(string(r.buf[from-r.base:]), "�")
}
