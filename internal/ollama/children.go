package ollama

import (
	"errors"
	"os"
	"sync"
)

// child is a server launch started in this process and has not seen exit.
type child struct {
	mu   sync.Mutex
	gone bool
	done chan struct{}
	proc *os.Process
}

var (
	childrenMu sync.Mutex
	children   = map[int]*child{}
)

func addChild(pid int) *child {
	c := &child{done: make(chan struct{})}
	childrenMu.Lock()
	children[pid] = c
	childrenMu.Unlock()
	return c
}

// childOf is the live child with id pid, if this process launched one.
func childOf(pid int) (*child, bool) {
	childrenMu.Lock()
	defer childrenMu.Unlock()
	c, ok := children[pid]
	return c, ok
}

// exit marks the child reaped, under its lock, so no signal follows.
func (c *child) exit() {
	c.mu.Lock()
	c.gone = true
	c.mu.Unlock()
	close(c.done)
	childrenMu.Lock()
	for pid, v := range children {
		if v == c {
			delete(children, pid)
		}
	}
	childrenMu.Unlock()
}

var errGone = errors.New("the server has already exited")

// signal runs send unless the child has been reaped.
func (c *child) signal(send func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return errGone
	}
	return send()
}
