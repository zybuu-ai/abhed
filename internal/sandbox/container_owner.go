package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
)

// serverOwnerLabel marks a server's container with the Abhed process that
// started it, as host:pid:start, so one a killed process left can be told
// from one a live process still runs.
const serverOwnerLabel = "abhed.mcp.owner"

// hostID names this machine in an owner label: a remote engine shows other
// hosts' containers, whose processes cannot be looked up here.
func hostID() string {
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:6])
}

// ownerOf is the owner label's value for process pid, or "" when its start
// cannot be read, where no container is marked as anyone's.
func ownerOf(pid int) string {
	start, ok := startTime(pid)
	if !ok {
		return ""
	}
	return hostID() + ":" + strconv.Itoa(pid) + ":" + strconv.FormatUint(start, 10)
}

// ownerGone reports a label naming a process on this host that has ended: its
// pid is free or taken by a later process. Another host's, or one that does
// not parse, is not gone.
func ownerGone(label string) bool {
	parts := strings.Split(label, ":")
	if len(parts) != 3 || parts[0] != hostID() {
		return false
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 0 {
		return false
	}
	return ownerOf(pid) != label
}

// serverOwnerArgs label a server's container as this process's.
func serverOwnerArgs() []string {
	if v := ownerOf(os.Getpid()); v != "" {
		return []string{"--label", serverOwnerLabel + "=" + v}
	}
	return nil
}

var sweepServersOnce sync.Once

// SweepServers removes, once per process, the server containers an Abhed
// process on this host left when it was killed, and returns their names.
// A container without the owner label is never touched.
func (c *Container) SweepServers(ctx context.Context) []string {
	var removed []string
	sweepServersOnce.Do(func() { removed = c.sweepServers(ctx) })
	return removed
}

func (c *Container) sweepServers(ctx context.Context) []string {
	out, err := c.engineCmd(ctx, "ps", "-a", "--filter", "label="+serverOwnerLabel, "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	var removed []string
	for _, name := range strings.Fields(string(out)) {
		// The label decides; the prefix only keeps the sweep to Abhed's own
		// names (a build before this one named a server's egress container so).
		if !strings.HasPrefix(name, serverPrefix) && !strings.HasPrefix(name, egressPrefix) {
			continue
		}
		label, err := c.engineCmd(ctx, "inspect", "--format", `{{index .Config.Labels "`+serverOwnerLabel+`"}}`, name).Output()
		if err != nil || !ownerGone(strings.TrimSpace(string(label))) {
			continue
		}
		c.removeNow(name)
		slog.Warn("removed an MCP server's container left by an Abhed process that ended", "container", name, "owner", strings.TrimSpace(string(label)))
		removed = append(removed, name)
	}
	return removed
}

// sweptNote tells the person of containers SweepServers removed.
func sweptNote(names []string) {
	if len(names) > 0 {
		fmt.Fprintf(os.Stderr, "abhed: removed %d MCP server container(s) an Abhed process left when it was killed: %s\n", len(names), strings.Join(names, ", "))
	}
}
