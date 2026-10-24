//go:build linux

package ollama

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

// memTotal is MemTotal from /proc/meminfo.
func memTotal() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || k != "MemTotal" {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			break
		}
		kb, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	return 0, errors.New("no MemTotal in /proc/meminfo")
}
