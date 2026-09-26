package tools

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"time"
)

// bashOutputWait is how long output is read after the command has ended.
const bashOutputWait = 2 * time.Second

// bashOutput collects a command's output through a pipe the call owns, so it
// can tell, whatever the exit status, when something left running holds it.
type bashOutput struct {
	r, w *os.File
	buf  bytes.Buffer
	done chan struct{}
}

func newBashOutput(cmd *exec.Cmd) (*bashOutput, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	o := &bashOutput{r: r, w: w, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = w, w
	go func() {
		defer close(o.done)
		_, _ = io.Copy(&o.buf, r)
	}()
	return o, nil
}

// run starts and waits for cmd, then reads what is left for up to delay. held
// reports a process that still had the output open when the delay ran out.
func (o *bashOutput) run(cmd *exec.Cmd, delay time.Duration) (out string, held bool, err error) {
	err = cmd.Start()
	_ = o.w.Close() // the command has its own copy; this one would hold the pipe open
	if err == nil {
		err = cmd.Wait()
	}
	select {
	case <-o.done:
	case <-time.After(delay):
		held = true
		_ = o.r.Close()
		<-o.done
	}
	_ = o.r.Close()
	return o.buf.String(), held, err
}
