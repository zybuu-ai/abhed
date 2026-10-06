package tools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// bashOutputWait is how long output is read after the command has ended.
const bashOutputWait = 2 * time.Second

// bashOutput collects a command's output through a pipe the call owns, so it
// can tell, whatever the exit status, when something left running holds it.
// Its output goes to buf until the command is moved to the background, then
// to the background shell's ring.
type bashOutput struct {
	r, w *os.File
	mu   sync.Mutex
	buf  headTail
	to   io.Writer // set once the command is moved to the background
	done chan struct{}
	// waited has cmd.Wait's result once the command has ended.
	waited chan error
}

func (o *bashOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.to != nil {
		return o.to.Write(p)
	}
	return o.buf.Write(p)
}

// moveTo sends what the command writes from now on to w, and returns what it
// wrote before: the call's own result.
func (o *bashOutput) moveTo(w io.Writer) (out string, truncated bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.to = w
	return o.buf.String(), o.buf.total > maxOutputChars
}

// headTail keeps the first and last half bytes (the intent and, usually, the
// error) and counts the rest, so a command printing gigabytes does not grow
// the server with it.
type headTail struct {
	half  int
	head  []byte
	tail  []byte // the last bytes written past head, trimmed lazily
	total int
}

func (h *headTail) Write(b []byte) (int, error) {
	n := len(b)
	h.total += n
	if room := h.half - len(h.head); room > 0 {
		take := min(room, len(b))
		h.head, b = append(h.head, b[:take]...), b[take:]
	}
	h.tail = append(h.tail, b...)
	if len(h.tail) > 2*h.half {
		h.tail = append(h.tail[:0:0], h.tail[len(h.tail)-h.half:]...)
	}
	return n, nil
}

// String is everything written when it fits in two halves; otherwise the head
// and the tail around a note of how much was left out.
func (h *headTail) String() string {
	if h.total <= 2*h.half {
		return string(h.head) + string(h.tail)
	}
	return fmt.Sprintf("%s\n\n[... %d characters truncated ...]\n\n%s",
		h.head, h.total-2*h.half, h.tail[len(h.tail)-h.half:])
}

func newBashOutput(cmd *exec.Cmd) (*bashOutput, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	o := &bashOutput{r: r, w: w, buf: headTail{half: maxOutputChars / 2}, done: make(chan struct{}), waited: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = w, w
	go func() {
		defer close(o.done)
		_, _ = io.Copy(o, r)
	}()
	return o, nil
}

// run starts and waits for cmd, then drains output for up to delay (held: still open after it);
// a receive on detach returns at once with detached set, the command going on for adopt.
func (o *bashOutput) run(cmd *exec.Cmd, delay time.Duration, started func(), detach <-chan struct{}) (out string, truncated, held, detached bool, err error) {
	err = cmd.Start()
	_ = o.w.Close() // the command has its own copy; this one would hold the pipe open
	if err != nil {
		sandbox.Release(cmd)
		_ = o.r.Close()
		<-o.done
		return o.buf.String(), false, false, false, err
	}
	go func() { o.waited <- cmd.Wait() }()
	if started != nil {
		started()
	}
	select {
	case err = <-o.waited:
	case <-detach:
		return "", false, false, true, nil
	}
	select {
	case <-o.done:
	case <-time.After(delay):
		held = true
		_ = o.r.Close()
		<-o.done
	}
	_ = o.r.Close()
	return o.buf.String(), o.buf.total > maxOutputChars, held, false, err
}

// adopt makes the still-running command a background shell whose output, from
// now on, is kept in a ring of capBytes; stop must end it. out is what it
// wrote before, which stays the call's result.
func (o *bashOutput) adopt(cmd *exec.Cmd, stop func(), capBytes int) (p *ShellProc, out string, truncated bool) {
	if capBytes <= 0 {
		capBytes = ShellOutputCap
	}
	p = &ShellProc{cmd: cmd, stop: stop, out: &shellRing{max: capBytes}, done: make(chan struct{}), started: time.Now()}
	out, truncated = o.moveTo(p.out)
	liveProcs.Store(p, struct{}{})
	go p.watch(o.waited, o.done, o.r)
	return p, out, truncated
}
