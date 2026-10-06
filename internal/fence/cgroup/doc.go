// Package cgroup bounds the commands the fence runs with Linux cgroup v2.
//
// Abhed never takes a cgroup it was not given. Discover finds the delegated
// subtree this process may manage: its own cgroup, when systemd delegated it
// (a user scope with Delegate=yes) or a container runtime did. Without one it
// returns an error that says how to get one, and the caller refuses to run:
// nothing here falls back to running a command unbounded.
//
// The layout below the delegated cgroup D is
//
//	D/abhed-host/              the processes that were in D, Abhed among them
//	D/abhed-sandbox-<id>/      one Sandbox: cpu.max, memory.max, memory.swap.max,
//	                           pids.max, memory.oom.group and io.max
//	D/abhed-sandbox-<id>/call-<call id>/   one Leaf per tool call
//
// A Leaf places a command at its start, through clone3's CLONE_INTO_CGROUP,
// so no process of the command runs outside it, not even briefly. A Sandbox or
// a Leaf can be frozen, thawed and killed as a whole, and reports the OOM and
// pids-limit events the record shows. Remove kills what is left and removes
// the cgroups.
//
// Only Linux has cgroups; elsewhere Discover returns ErrUnsupported. The
// limit types and their parsing are the same on every system.
package cgroup
