package landlock

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const user = 1000

func baseSpec() Spec {
	return Spec{
		Exec:    []string{"/usr", "/bin", "/etc"},
		Read:    []string{"/proc"},
		Write:   []string{"/fence-test/u/work", "/tmp/abhed-cmd-1"},
		Devices: []string{"/dev/null"},
		Deny:    []string{"/fence-test/u/.abhed", "/fence-test/u/.config/abhed/secrets.env"},
	}
}

func TestValidateAcceptsDisjointSpec(t *testing.T) {
	if err := baseSpec().Validate(); err != nil {
		t.Fatalf("a spec whose denied paths sit outside every grant was refused: %v", err)
	}
	if _, err := newRuleset(baseSpec(), 5, user, user); err != nil {
		t.Fatalf("New refused a valid spec on ABI 5: %v", err)
	}
}

func TestValidateRefusesOverlap(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"workspace .abhed under write grant", func(s *Spec) { s.Deny = append(s.Deny, "/fence-test/u/work/.abhed") }, "carve"},
		{"home as workspace", func(s *Spec) { s.Write = append(s.Write, "/fence-test/u") }, "carve"},
		{"root write grant", func(s *Spec) { s.Write = []string{"/"} }, "carve"},
		{"denied path equal to a grant", func(s *Spec) { s.Write = append(s.Write, "/fence-test/u/.abhed") }, "under the write grant"},
		{"denied path under a read grant", func(s *Spec) { s.Read = append(s.Read, "/fence-test") }, "readable"},
		{"denied path under an exec grant", func(s *Spec) { s.Exec = append(s.Exec, "/fence-test/u/.config") }, "readable"},
		{"grant inside denied state", func(s *Spec) { s.Read = append(s.Read, "/fence-test/u/.abhed/logs") }, "inside the denied path"},
		{"unclean path still overlaps", func(s *Spec) { s.Write = append(s.Write, "/fence-test/u/work/../") }, "carve"},
		{"relative grant", func(s *Spec) { s.Write = append(s.Write, "work") }, "not absolute"},
		{"relative denial", func(s *Spec) { s.Deny = append(s.Deny, ".abhed") }, "not absolute"},
		{"empty path", func(s *Spec) { s.Exec = append(s.Exec, "") }, "not absolute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := baseSpec()
			c.edit(&s)
			err := s.Validate()
			if !errors.Is(err, ErrSpec) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate() = %v, want ErrSpec mentioning %q", err, c.want)
			}
			if _, err := newRuleset(s, 5, user, user); !errors.Is(err, ErrSpec) {
				t.Fatalf("New accepted the spec: %v", err)
			}
		})
	}
}

// A link must not hide a denied path inside a write grant.
func TestValidateResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(target, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "work")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	s := Spec{Write: []string{link}, Deny: []string{filepath.Join(target, ".abhed")}}
	if err := s.Validate(); !errors.Is(err, ErrSpec) {
		t.Fatalf("a denied path reached through a linked write grant was accepted: %v", err)
	}
	// A denied path that does not exist yet still compares by where it lands.
	s = Spec{Write: []string{target}, Deny: []string{filepath.Join(link, "not-yet", "record")}}
	if err := s.Validate(); !errors.Is(err, ErrSpec) {
		t.Fatalf("a not-yet-created denied path behind a link was accepted: %v", err)
	}
}

func TestNewRefusesOldABI(t *testing.T) {
	for _, abi := range []ABI{0, 1, 2} {
		_, err := newRuleset(baseSpec(), abi, user, user)
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "Linux 6.2") {
			t.Fatalf("ABI %d: New = %v, want ErrUnsupported naming Linux 6.2", abi, err)
		}
	}
	if _, err := newRuleset(baseSpec(), MinABI, user, user); err != nil {
		t.Fatalf("ABI %d was refused: %v", MinABI, err)
	}
}

func TestNewRefusesTCPDenialWithoutNetworkABI(t *testing.T) {
	s := baseSpec()
	s.DenyTCP = true
	_, err := newRuleset(s, 3, user, user)
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "Linux 6.7") {
		t.Fatalf("DenyTCP on ABI 3: New = %v, want ErrUnsupported naming Linux 6.7", err)
	}
	if _, err := newRuleset(s, 4, user, user); err != nil {
		t.Fatalf("DenyTCP on ABI 4 was refused: %v", err)
	}
}

func TestNewRefusesRoot(t *testing.T) {
	for _, ids := range [][2]int{{0, 0}, {0, user}, {user, 0}} {
		if _, err := newRuleset(baseSpec(), 5, ids[0], ids[1]); !errors.Is(err, ErrRoot) {
			t.Fatalf("uid %d euid %d: New = %v, want ErrRoot", ids[0], ids[1], err)
		}
	}
}

func TestABIFeatures(t *testing.T) {
	cases := []struct {
		abi              ABI
		rights           int
		network, scoping bool
		hasTruncate      bool
		hasIoctl         bool
	}{
		{0, 0, false, false, false, false},
		{1, 13, false, false, false, false},
		{3, 15, false, false, true, false},
		{4, 15, true, false, true, false},
		{5, 16, true, false, true, true},
		{6, 16, true, true, true, true},
	}
	for _, c := range cases {
		got := c.abi.FSRights()
		if len(got) != c.rights || c.abi.Network() != c.network || c.abi.Scoping() != c.scoping {
			t.Errorf("ABI %d: rights %v network %v scoping %v", c.abi, got, c.abi.Network(), c.abi.Scoping())
		}
		joined := strings.Join(got, ",")
		if strings.Contains(joined, "truncate") != c.hasTruncate || strings.Contains(joined, "ioctl_dev") != c.hasIoctl {
			t.Errorf("ABI %d: rights %v", c.abi, got)
		}
	}
}

func TestDescribe(t *testing.T) {
	if d := ABI(0).Describe(); !strings.Contains(d, "not available") {
		t.Errorf("ABI 0: %q", d)
	}
	if d := ABI(2).Describe(); !strings.Contains(d, "too old") {
		t.Errorf("ABI 2: %q", d)
	}
	d := ABI(5).Describe()
	for _, want := range []string{"ABI 5 (Linux 6.10)", "truncate", "ioctl_dev", "TCP bind and connect", "no signal scoping (ABI 6 (Linux 6.12))"} {
		if !strings.Contains(d, want) {
			t.Errorf("ABI 5 description %q lacks %q", d, want)
		}
	}
	s := baseSpec()
	s.DenyTCP = true
	r, err := newRuleset(s, 6, user, user)
	if err != nil {
		t.Fatal(err)
	}
	d = r.Describe()
	for _, want := range []string{"ABI 6", "read and write /fence-test/u/work, /tmp/abhed-cmd-1", "denied /fence-test/u/.abhed", "TCP connect and bind refused", "scoped"} {
		if !strings.Contains(d, want) {
			t.Errorf("ruleset description %q lacks %q", d, want)
		}
	}
	if r.ABI() != 6 {
		t.Errorf("ABI() = %d", r.ABI())
	}
}

func TestRestrictRefusesOffLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux applies the ruleset; landlock_linux_test.go covers it")
	}
	if abi, err := Detect(); abi != 0 || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Detect() = %d, %v", abi, err)
	}
	r := &Ruleset{spec: baseSpec(), abi: 5}
	if err := r.Restrict(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Restrict() = %v, want ErrUnsupported", err)
	}
}

// An exec grant named in Within may sit inside denied state, read and run
// only; it may not hold the denied path, and it must be an exec grant.
func TestValidateWithin(t *testing.T) {
	ok := Spec{Exec: []string{"/usr", "/home/u/.abhed/skills"}, Deny: []string{"/home/u/.abhed"}, Within: []string{"/home/u/.abhed/skills"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("skills inside state: %v", err)
	}
	for name, s := range map[string]Spec{
		"not listed":    {Exec: []string{"/home/u/.abhed/skills"}, Deny: []string{"/home/u/.abhed"}},
		"read grant":    {Read: []string{"/home/u/.abhed/skills"}, Deny: []string{"/home/u/.abhed"}, Within: []string{"/home/u/.abhed/skills"}},
		"holds state":   {Exec: []string{"/home/u/.abhed"}, Deny: []string{"/home/u/.abhed/users.json"}, Within: []string{"/home/u/.abhed"}},
		"write grant":   {Write: []string{"/home/u/.abhed/skills"}, Exec: []string{"/home/u/.abhed/skills"}, Deny: []string{"/home/u/.abhed"}, Within: []string{"/home/u/.abhed/skills"}},
		"same as state": {Exec: []string{"/home/u/.abhed"}, Deny: []string{"/home/u/.abhed"}, Within: []string{"/home/u/.abhed"}},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}
