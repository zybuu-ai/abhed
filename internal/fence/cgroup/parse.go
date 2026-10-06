package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Events are the counts the kernel keeps for a cgroup and its descendants.
type Events struct {
	// MemoryHigh and MemoryMax count the times memory.high throttled and
	// memory.max was about to be passed.
	MemoryHigh, MemoryMax uint64
	// OOM counts the times memory.max was reached and reclaim failed;
	// OOMKill the processes the OOM killer took; OOMGroupKill the times it
	// took the whole cgroup, with memory.oom.group set.
	OOM, OOMKill, OOMGroupKill uint64
	// PidsMax counts the forks refused at pids.max.
	PidsMax uint64
}

// keyed reads a flat-keyed cgroup file: one "key value" per line.
func keyed(text string) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out
}

// eventsFrom fills Events from memory.events and pids.events.
func eventsFrom(memory, pids string) Events {
	m, p := keyed(memory), keyed(pids)
	return Events{
		MemoryHigh:   m["high"],
		MemoryMax:    m["max"],
		OOM:          m["oom"],
		OOMKill:      m["oom_kill"],
		OOMGroupKill: m["oom_group_kill"],
		PidsMax:      p["max"],
	}
}

// ownPath is the cgroup v2 path in /proc/self/cgroup's text: the "0::" line.
func ownPath(procCgroup string) (string, error) {
	for _, line := range strings.Split(procCgroup, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && strings.HasPrefix(p, "/") {
			return path.Clean(p), nil
		}
	}
	return "", errors.New("this process is in no cgroup v2 hierarchy (no 0:: line in /proc/self/cgroup)")
}

// mountOf is the cgroup2 mount point and its root in mountinfo's text.
func mountOf(mountinfo string) (point, root string, err error) {
	for _, line := range strings.Split(mountinfo, "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		if f := strings.Fields(post); len(f) == 0 || f[0] != "cgroup2" {
			continue
		}
		f := strings.Fields(pre)
		if len(f) < 5 {
			continue
		}
		return unescape(f[4]), unescape(f[3]), nil
	}
	return "", "", errors.New("no cgroup2 file system is mounted (cgroup v1 only)")
}

// unescape undoes mountinfo's octal escapes, "\040" for a space.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// dirOf is the directory of cgroup own under a cgroup2 mount at point whose
// root is root.
func dirOf(point, root, own string) (string, error) {
	rel, ok := strings.CutPrefix(own, root)
	if root == "/" {
		rel, ok = own, true
	}
	if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
		return "", fmt.Errorf("cgroup %s is outside the mounted cgroup2 root %s", own, root)
	}
	return path.Join(point, rel), nil
}

// words is a space-separated cgroup file, cgroup.controllers say, as a set.
func words(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(text) {
		out[w] = true
	}
	return out
}

// pids are the process ids in a cgroup.procs file.
func pids(text string) []int {
	var out []int
	for _, f := range strings.Fields(text) {
		if n, err := strconv.Atoi(f); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// validName reports whether id can name a sandbox or a call: letters, digits,
// '.', '_' and '-', not starting with '.', at most 64 bytes.
func validName(id string) bool {
	if id == "" || len(id) > 64 || id[0] == '.' {
		return false
	}
	for _, r := range id {
		ok := r == '.' || r == '_' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}
