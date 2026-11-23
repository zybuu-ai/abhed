package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ssh-acp refuses before it connects: a malformed account, an unpinned or
// relative engine, a relative folder, a local build without the pinned hash,
// and any run inside an agent's command.
func TestSSHACPRefusals(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	build := filepath.Join(t.TempDir(), "abhed-linux")
	if err := os.WriteFile(build, []byte("not this hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		args []string
		want string
	}{
		"no folder":       {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed"}, "usage"},
		"bad user":        {[]string{"-host", "h", "-user", "-oProxyCommand=x", "-sha256", sha, "-engine", "/opt/abhed", "--", "/w"}, "SSH account"},
		"bad host":        {[]string{"-host", "-oProxyCommand=x", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed", "--", "/w"}, "SSH account"},
		"no pin":          {[]string{"-host", "h", "-user", "u", "-engine", "/opt/abhed", "--", "/w"}, "SHA-256"},
		"relative folder": {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed", "--", "w"}, "absolute path on the host"},
		"relative engine": {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "abhed", "--", "/w"}, "-engine is an absolute"},
		"no engine":       {[]string{"-host", "h", "-user", "u", "-sha256", sha, "--", "/w"}, "give -engine"},
		"upload mismatch": {[]string{"-host", "h", "-user", "u", "-sha256", sha, "-upload", build, "--", "/w"}, "does not have the pinned SHA-256"},
	}
	for name, c := range cases {
		var out, errw bytes.Buffer
		if code := sshACPCmd(c.args, strings.NewReader(""), &out, &errw); code == 0 || !strings.Contains(errw.String(), c.want) {
			t.Errorf("%s: %d %q", name, code, errw.String())
		}
	}
	old := inAgentCommand
	t.Cleanup(func() { inAgentCommand = old })
	inAgentCommand = func() string { return "ABHED_SANDBOX is set" }
	var errw bytes.Buffer
	if code := sshACPCmd([]string{"-host", "h", "-user", "u", "-sha256", sha, "-engine", "/opt/abhed", "--", "/w"}, strings.NewReader(""), &bytes.Buffer{}, &errw); code == 0 || !strings.Contains(errw.String(), "inside an agent") {
		t.Fatalf("inside an agent: %q", errw.String())
	}
}

// The one remote step, run here with sh against a stand-in engine: the pinned
// binary is copied into a private folder, checked and exec'd from there; a
// home or engine folder others can write, a planted .part link and a wrong
// hash are refused, and a path with shell syntax in it is printed, not run.
func TestSSHStartScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "engine")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"started $1 in $(pwd)\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(readOrFail(t, stub))
	want := hex.EncodeToString(sum[:])
	run := func(home, script string, stdin []byte) (string, int) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
		cmd.Stdin = bytes.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	newHome := func() string {
		h := filepath.Join(t.TempDir(), "home")
		if err := os.MkdirAll(h, 0o755); err != nil {
			t.Fatal(err)
		}
		return h
	}
	folder := t.TempDir()

	home := newHome()
	out, code := run(home, sshStartScript(stub, want, folder), nil)
	if code != 0 || !strings.Contains(out, "started acp") {
		t.Fatalf("start: %d %s", code, out)
	}
	copyPath := filepath.Join(home, ".abhed", "bin", "abhed-"+want[:16])
	if info, err := os.Stat(copyPath); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("the private copy: %v %v", info, err)
	}
	if info, _ := os.Stat(filepath.Join(home, ".abhed", "bin")); info.Mode().Perm() != 0o700 {
		t.Fatalf("~/.abhed/bin is %v", info.Mode())
	}
	// A swapped copy is replaced by the pinned binary before it runs.
	_ = os.Chmod(copyPath, 0o700)
	_ = os.WriteFile(copyPath, []byte("#!/bin/sh\necho swapped\n"), 0o700)
	if out, code := run(home, sshStartScript(stub, want, folder), nil); code != 0 || strings.Contains(out, "swapped") {
		t.Fatalf("a swapped copy ran: %d %s", code, out)
	}
	// A wrong pin.
	if out, code := run(newHome(), sshStartScript(stub, strings.Repeat("e", 64), folder), nil); code != 97 || !strings.Contains(out, "does not have the pinned SHA-256") {
		t.Fatalf("wrong pin: %d %s", code, out)
	}
	// A home others can write.
	lax := newHome()
	_ = os.Chmod(lax, 0o775)
	if out, code := run(lax, sshStartScript(stub, want, folder), nil); code != 97 || !strings.Contains(out, "writable by others") {
		t.Fatalf("a group-writable home: %d %s", code, out)
	}
	// A folder above the home that others can write.
	above := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(filepath.Join(above, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(above, 0o777)
	if out, code := run(filepath.Join(above, "home"), sshStartScript(stub, want, folder), nil); code != 97 || !strings.Contains(out, "above your home folder") {
		t.Fatalf("a folder above the home others can write: %d %s", code, out)
	}
	// A planted .part link.
	planted := newHome()
	if err := os.MkdirAll(filepath.Join(planted, ".abhed", "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, filepath.Join(planted, ".abhed", "bin", "abhed-"+want[:16]+".part")); err != nil {
		t.Fatal(err)
	}
	if out, code := run(planted, sshStartScript(stub, want, folder), nil); code != 97 || !strings.Contains(out, "is a link") {
		t.Fatalf("a planted .part link: %d %s", code, out)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Fatal("the link's target was written")
	}
	// Shell syntax in the path is printed as data.
	marker := filepath.Join(t.TempDir(), "ran")
	if out, code := run(newHome(), sshStartScript("/nope/$(touch "+marker+")", want, folder), nil); code != 97 || !strings.Contains(out, "there is no engine at") {
		t.Fatalf("a missing engine: %d %s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the path's shell syntax ran")
	}
	// Upload: from stdin into the private folder, then started from there.
	up := newHome()
	if out, code := run(up, sshPutScript(want), readOrFail(t, stub)); code != 0 {
		t.Fatalf("put: %d %s", code, out)
	}
	if out, code := run(up, sshStartScript("", want, folder), nil); code != 0 || !strings.Contains(out, "started acp") {
		t.Fatalf("start after upload: %d %s", code, out)
	}
	if out, code := run(newHome(), sshPutScript(want), []byte("not the engine")); code != 97 || !strings.Contains(out, "pinned SHA-256") {
		t.Fatalf("a wrong upload: %d %s", code, out)
	}
}

func readOrFail(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
