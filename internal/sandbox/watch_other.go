//go:build !unix

package sandbox

import (
	"os/exec"
	"sync"
)

// Watch, here, only waits: the process table is not read on this platform.
type Watch struct{ cmd *exec.Cmd }

func StartWatch(cmd *exec.Cmd) *Watch { return &Watch{cmd: cmd} }

func (w *Watch) Wait() (string, error) { return "", w.cmd.Wait() }

type commandSet struct{ mu sync.Mutex }

func (c *commandSet) add(*exec.Cmd) {}

func (c *commandSet) end() {}
