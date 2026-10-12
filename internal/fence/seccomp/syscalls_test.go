package seccomp

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every table agrees with golang.org/x/sys/unix, read from the module's source
// so it holds on any host; the native test checks compiled constants.
func TestTablesMatchXSys(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "golang.org/x/sys").Output()
	if err != nil {
		t.Fatalf("finding golang.org/x/sys with the go command: %v", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" {
		t.Fatal("golang.org/x/sys is not in the module cache; run go mod download")
	}
	dir := filepath.Join(mod, "unix")
	audit := readConsts(t, filepath.Join(dir, "zerrors_linux.go"), "AUDIT_ARCH_")
	auditName := map[string]string{"amd64": "AUDIT_ARCH_X86_64", "arm64": "AUDIT_ARCH_AARCH64", "s390x": "AUDIT_ARCH_S390X"}
	for _, arch := range archNames {
		nums := readConsts(t, filepath.Join(dir, "zsysnum_linux_"+arch+".go"), "SYS_")
		for name, n := range arches[arch].nr {
			x, ok := nums["SYS_"+strings.ToUpper(name)]
			if !ok {
				t.Errorf("%s: x/sys has no SYS_%s", arch, strings.ToUpper(name))
				continue
			}
			if x != uint64(n) {
				t.Errorf("%s: %s is %d in the table, %d in x/sys", arch, name, n, x)
			}
		}
		if a := audit[auditName[arch]]; a != uint64(arches[arch].audit) {
			t.Errorf("%s: audit arch %#x, x/sys %s %#x", arch, arches[arch].audit, auditName[arch], a)
		}
	}
	// The flags the table carries follow from the kernel's tables.
	for _, arch := range archNames {
		nums := readConsts(t, filepath.Join(dir, "zsysnum_linux_"+arch+".go"), "SYS_")
		_, sc := nums["SYS_SOCKETCALL"]
		_, um := nums["SYS_UMOUNT"]
		if sc != arches[arch].socketcall || um != arches[arch].umount {
			t.Errorf("%s: x/sys has socketcall %v and umount %v, the table %v and %v", arch, sc, um, arches[arch].socketcall, arches[arch].umount)
		}
	}
}

var constLine = regexp.MustCompile(`^\s*([A-Z0-9_]+)\s*=\s*(0x[0-9a-fA-F]+|[0-9]+)\s*$`)

func readConsts(t *testing.T, path, prefix string) map[string]uint64 {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- a file in the x/sys module the build uses
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	m := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		g := constLine.FindStringSubmatch(sc.Text())
		if g == nil || !strings.HasPrefix(g[1], prefix) {
			continue
		}
		v, err := strconv.ParseUint(g[2], 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		m[g[1]] = v
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(m) == 0 {
		t.Fatalf("no %s constants in %s", prefix, path)
	}
	return m
}
