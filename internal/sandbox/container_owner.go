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

// existence is what the kernel says of a pid.
type existence int

const (
	existsUnknown existence = iota
	existsYes
	existsNo
)

// ownerExists and ownerStart read an owner's process; tests replace them.
var (
	ownerExists = processExists
	ownerStart  = startTime
)

// What a sweep can say of an owner label.
type ownerState int

const (
	ownerLives     ownerState = iota // the process runs, as labelled
	ownerEnded                       // its pid is free or taken by a later process
	ownerUnchecked                   // its process cannot be read here (another user's under hidepid)
	ownerElsewhere                   // another host's, or a label that does not parse
)

// ownerOfLabel says whether a label names a process on this host that has
// ended. Only a proof counts: a process that cannot be read is not ended.
func ownerOfLabel(label string) ownerState {
	parts := strings.Split(label, ":")
	if len(parts) != 3 || parts[0] != hostID() {
		return ownerElsewhere
	}
	pid, err := strconv.Atoi(parts[1])
	want, perr := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || perr != nil || pid <= 0 {
		return ownerElsewhere
	}
	switch ownerExists(pid) {
	case existsNo:
		return ownerEnded
	case existsUnknown:
		return ownerUnchecked
	}
	start, ok := ownerStart(pid)
	switch {
	case !ok:
		return ownerUnchecked
	case start != want:
		return ownerEnded
	}
	return ownerLives
}

// serverOwnerArgs label a server's container as this process's.
func serverOwnerArgs() []string {
	if v := ownerOf(os.Getpid()); v != "" {
		return []string{"--label", serverOwnerLabel + "=" + v}
	}
	return nil
}

var sweepServersOnce sync.Once

// ServerSweep is what a sweep did: the containers it removed, and those it
// kept because their owner could not be checked here.
type ServerSweep struct{ Removed, Unchecked []string }

// SweepServers removes, once per process, the server containers an Abhed
// process on this host left when it was killed. A container without the owner
// label, or whose owner cannot be read, is never touched.
func (c *Container) SweepServers(ctx context.Context) ServerSweep {
	var sw ServerSweep
	sweepServersOnce.Do(func() { sw = c.sweepServers(ctx) })
	return sw
}

func (c *Container) sweepServers(ctx context.Context) ServerSweep {
	var sw ServerSweep
	out, err := c.engineCmd(ctx, "ps", "-a", "--filter", "label="+serverOwnerLabel, "--format", "{{.Names}}").Output()
	if err != nil {
		return sw
	}
	for _, name := range strings.Fields(string(out)) {
		// The label decides; the prefix only keeps the sweep to Abhed's own
		// names (a build before this one named a server's egress container so).
		if !strings.HasPrefix(name, serverPrefix) && !strings.HasPrefix(name, egressPrefix) {
			continue
		}
		raw, err := c.engineCmd(ctx, "inspect", "--format", `{{index .Config.Labels "`+serverOwnerLabel+`"}}`, name).Output()
		if err != nil {
			continue
		}
		label := strings.TrimSpace(string(raw))
		switch ownerOfLabel(label) {
		case ownerEnded:
			c.removeNow(name)
			slog.Warn("removed an MCP server's container left by an Abhed process that ended", "container", name, "owner", label)
			sw.Removed = append(sw.Removed, name)
		case ownerUnchecked:
			sw.Unchecked = append(sw.Unchecked, name)
		}
	}
	return sw
}

// sweptNote tells the person of containers SweepServers removed.
func sweptNote(sw ServerSweep) {
	if len(sw.Removed) > 0 {
		fmt.Fprintf(os.Stderr, "abhed: removed %d MCP server container(s) an Abhed process left when it was killed: %s\n", len(sw.Removed), strings.Join(sw.Removed, ", "))
	}
}
