package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/driver"
	"github.com/zybuu-ai/abhed/internal/fence/cgroup"
)

// hostSizer gives each sized instance a cgroup of its own under the
// delegated subtree, and each of its commands a leaf inside it.
var hostSizer sizing = &cgroupSizer{}

type cgroupSizer struct {
	once sync.Once
	base *cgroup.Base
	err  error
}

func (c *cgroupSizer) discover() (*cgroup.Base, error) {
	c.once.Do(func() { c.base, c.err = cgroup.Discover() })
	return c.base, c.err
}

func (c *cgroupSizer) Check() error {
	if _, err := c.discover(); err != nil {
		return driver.Errorf(driver.ErrInvalid, "the process driver bounds cpus and memory with cgroups, and none can be made here: %v", err)
	}
	return nil
}

// boxPrefix keeps these cgroups apart from the fence's, made in the same subtree.
const boxPrefix = "pd-"

func (c *cgroupSizer) NewBox(id string, s size) (box, error) {
	base, err := c.discover()
	if err != nil {
		return nil, err
	}
	l := cgroup.Limits{CPUPercent: s.cpuPercent, MemoryMax: s.memoryBytes, OOMGroup: s.memoryBytes > 0}
	// A box left by a server that stopped without deleting it is not this one.
	_ = base.RemoveSandbox(context.Background(), boxPrefix+id)
	sb, err := base.NewSandbox(boxPrefix+id, l)
	if err != nil {
		return nil, err
	}
	return &cgroupBox{sb: sb}, nil
}

type cgroupBox struct {
	sb *cgroup.Sandbox
	n  atomic.Int64
}

func (b *cgroupBox) Place(cmd *exec.Cmd) (func(), error) {
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.UseCgroupFD {
		return nil, errors.New("the command already has a cgroup of its own")
	}
	leaf, err := b.sb.NewLeaf("c"+strconv.FormatInt(b.n.Add(1), 10), cgroup.Limits{})
	if err != nil {
		return nil, err
	}
	if err := leaf.Place(cmd); err != nil {
		_ = leaf.Remove(context.Background())
		return nil, fmt.Errorf("placing the command: %w", err)
	}
	var once sync.Once
	return func() { once.Do(func() { _ = leaf.Remove(context.Background()) }) }, nil
}

func (b *cgroupBox) Remove(ctx context.Context) error { return b.sb.Remove(ctx) }
