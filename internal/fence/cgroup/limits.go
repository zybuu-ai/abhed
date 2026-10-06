package cgroup

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Max asks for the kernel's "max", no limit, written explicitly. A zero
// leaves the value the cgroup inherits, except for SwapMax.
const Max = -1

// cpuPeriod is the cpu.max period in microseconds, the kernel's default.
const cpuPeriod = 100000

// minMemory is the smallest memory.max accepted: below it a command dies at
// once, which is a mistake in the configuration rather than a bound.
const minMemory = 1 << 20

// maxCPUPercent is 1024 CPUs, far above any host; beyond it is a typo.
const maxCPUPercent = 100 * 1024

// Limits are the bounds of a Sandbox or a Leaf. A zero field is not written.
type Limits struct {
	// CPUPercent is the CPU time allowed, in percent of one CPU: 50 is half
	// of one, 200 two whole ones.
	CPUPercent int
	// MemoryMax is memory.max in bytes.
	MemoryMax int64
	// SwapMax is memory.swap.max in bytes. With MemoryMax set, a zero means
	// no swap: a memory bound that can spill into swap is not a bound.
	SwapMax int64
	// PidsMax is pids.max: processes and threads together.
	PidsMax int64
	// OOMGroup sets memory.oom.group, so that an OOM kill takes the whole
	// cgroup rather than one process of it.
	OOMGroup bool
	// IO are io.max lines, one per device.
	IO []IOMax
}

// IOMax bounds one block device in io.max. A zero rate is not written.
type IOMax struct {
	Major, Minor uint32
	// RBps and WBps are bytes per second; RIOps and WIOps operations.
	RBps, WBps, RIOps, WIOps int64
}

// ErrInvalidLimits is wrapped by every Validate and parse error.
var ErrInvalidLimits = errors.New("cgroup: invalid limits")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidLimits}, a...)...)
}

// Validate checks every field is in range.
func (l Limits) Validate() error {
	if l.CPUPercent < 0 || l.CPUPercent > maxCPUPercent {
		return invalid("cpu percent %d is outside 0 to %d", l.CPUPercent, maxCPUPercent)
	}
	if l.MemoryMax != 0 && l.MemoryMax != Max && l.MemoryMax < minMemory {
		return invalid("memory.max %d is below %d bytes", l.MemoryMax, minMemory)
	}
	if l.SwapMax < Max {
		return invalid("memory.swap.max %d is negative", l.SwapMax)
	}
	if l.PidsMax < Max {
		return invalid("pids.max %d is negative", l.PidsMax)
	}
	seen := map[[2]uint32]bool{}
	for _, d := range l.IO {
		if err := d.validate(); err != nil {
			return err
		}
		key := [2]uint32{d.Major, d.Minor}
		if seen[key] {
			return invalid("io.max names device %d:%d twice", d.Major, d.Minor)
		}
		seen[key] = true
	}
	return nil
}

func (d IOMax) validate() error {
	set := false
	for _, v := range []int64{d.RBps, d.WBps, d.RIOps, d.WIOps} {
		if v < Max {
			return invalid("io.max rate %d for %d:%d is negative", v, d.Major, d.Minor)
		}
		set = set || v != 0
	}
	if !set {
		return invalid("io.max for %d:%d sets no rate", d.Major, d.Minor)
	}
	return nil
}

// line is d as io.max takes it.
func (d IOMax) line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d:%d", d.Major, d.Minor)
	for _, kv := range []struct {
		k string
		v int64
	}{{"rbps", d.RBps}, {"wbps", d.WBps}, {"riops", d.RIOps}, {"wiops", d.WIOps}} {
		if kv.v != 0 {
			b.WriteString(" " + kv.k + "=" + value(kv.v))
		}
	}
	return b.String()
}

// value is n as a cgroup file takes it.
func value(n int64) string {
	if n == Max {
		return "max"
	}
	return strconv.FormatInt(n, 10)
}

// setting is one write to an interface file of a cgroup.
type setting struct {
	file, value string
}

// settings are the writes l asks for, in order: memory.max before the swap
// bound that depends on it, and the group kill before anything can OOM.
func (l Limits) settings() []setting {
	var out []setting
	if l.CPUPercent > 0 {
		quota := int64(l.CPUPercent) * cpuPeriod / 100
		out = append(out, setting{"cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriod)})
	}
	if l.OOMGroup {
		out = append(out, setting{"memory.oom.group", "1"})
	}
	if l.MemoryMax != 0 {
		out = append(out, setting{"memory.max", value(l.MemoryMax)})
	}
	if l.SwapMax != 0 || (l.MemoryMax != 0 && l.MemoryMax != Max) {
		out = append(out, setting{"memory.swap.max", value(l.SwapMax)})
	}
	if l.PidsMax != 0 {
		out = append(out, setting{"pids.max", value(l.PidsMax)})
	}
	for _, d := range l.IO {
		out = append(out, setting{"io.max", d.line()})
	}
	return out
}

// controllers are the controllers l's settings need.
func (l Limits) controllers() []string {
	var out []string
	add := func(c string) {
		for _, have := range out {
			if have == c {
				return
			}
		}
		out = append(out, c)
	}
	for _, s := range l.settings() {
		add(strings.SplitN(s.file, ".", 2)[0])
	}
	return out
}

// ParseSize reads a size as a configuration gives it: "max", a byte count, or
// a count with K, M, G or T (powers of 1024, with an optional "i" and "B").
// An empty string is zero, which leaves the limit unset.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "":
		return 0, nil
	case "max":
		return Max, nil
	}
	num := strings.TrimRight(s, "KMGTkmgtiB")
	unit := strings.TrimSuffix(s[len(num):], "B")
	if len(unit) == 2 && unit[1] == 'i' {
		unit = unit[:1]
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}
	sh, ok := shift[strings.ToUpper(unit)]
	if !ok || num == "" {
		return 0, invalid("size %q: want a number with K, M, G or T, or max", s)
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, invalid("size %q: want a whole, positive number", s)
	}
	if n > math.MaxInt64>>sh {
		return 0, invalid("size %q is too large", s)
	}
	return n << sh, nil
}

// ParseIOMax reads one line in io.max's own form: "MAJ:MIN" followed by
// rbps=, wbps=, riops= and wiops= values, each a number or max.
func ParseIOMax(s string) (IOMax, error) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return IOMax{}, invalid("io.max %q: want MAJ:MIN and at least one key=value", s)
	}
	var d IOMax
	majS, minS, ok := strings.Cut(fields[0], ":")
	maj, err1 := strconv.ParseUint(majS, 10, 32)
	minr, err2 := strconv.ParseUint(minS, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return IOMax{}, invalid("io.max %q: device %q is not MAJ:MIN", s, fields[0])
	}
	d.Major, d.Minor = uint32(maj), uint32(minr)
	for _, f := range fields[1:] {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return IOMax{}, invalid("io.max %q: %q is not key=value", s, f)
		}
		n, err := ParseSize(v)
		if err != nil || n == 0 {
			return IOMax{}, invalid("io.max %q: bad value %q", s, v)
		}
		switch k {
		case "rbps":
			d.RBps = n
		case "wbps":
			d.WBps = n
		case "riops":
			d.RIOps = n
		case "wiops":
			d.WIOps = n
		default:
			return IOMax{}, invalid("io.max %q: unknown key %q", s, k)
		}
	}
	return d, d.validate()
}
