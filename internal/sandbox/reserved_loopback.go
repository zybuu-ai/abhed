package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// reservedLoopback is the loopback port range Abhed's own listeners hold on
// the Mac, which no command may bind: ABHED_LOOPBACK_PORTS ("lo-hi", at
// least 16 ports above 1024), else 47600-47855. It matches internal/loopport.
func reservedLoopback() (lo, hi int) {
	lo, hi = 47600, 47855
	if v := os.Getenv("ABHED_LOOPBACK_PORTS"); v != "" {
		a, b, ok := strings.Cut(v, "-")
		l, err1 := strconv.Atoi(strings.TrimSpace(a))
		h, err2 := strconv.Atoi(strings.TrimSpace(b))
		if ok && err1 == nil && err2 == nil && l > 1024 && h <= 65535 && h-l+1 >= 16 {
			lo, hi = l, h
		}
	}
	return lo, hi
}

// hostListeners are the TCP ports this user's processes listen on now, any
// address. A command binding loopback on one would take the connections
// meant for it (a wildcard listener leaves 127.0.0.1 free to bind), so
// those ports are not bindable; another user's the kernel itself refuses.
// ok is false when they cannot be read: a listener of our own, opened
// first, must be among them.
func hostListeners() (ports []int, ok bool) {
	listeners.mu.Lock()
	defer listeners.mu.Unlock()
	if time.Since(listeners.at) < 2*time.Second && listeners.ok {
		return listeners.ports, true
	}
	own, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, false
	}
	defer own.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	if err != nil {
		return nil, false
	}
	ports = parseListeners(string(out))
	if !slices.Contains(ports, own.Addr().(*net.TCPAddr).Port) {
		return nil, false
	}
	listeners.ports, listeners.ok, listeners.at = ports, true, time.Now()
	return ports, true
}

var listeners struct {
	mu    sync.Mutex
	at    time.Time
	ok    bool
	ports []int
}

// parseListeners reads lsof -Fn's name lines: n*:5432, n127.0.0.1:8080,
// n[::1]:8080.
func parseListeners(out string) []int {
	seen := map[int]bool{}
	var ports []int
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		i := strings.LastIndexByte(line, ':')
		if i < 0 {
			continue
		}
		p, err := strconv.Atoi(line[i+1:])
		if err != nil || p <= 0 || p > 65535 || seen[p] {
			continue
		}
		seen[p] = true
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}
