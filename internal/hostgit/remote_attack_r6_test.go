package hostgit

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// D8: include.path = ~user/... (git expands it as the named user's home); the walk
// treats it as relative and checks a different, absent file.
func TestR6TildeUserInclude(t *testing.T) {
	r := newRemoteRig(t)
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	realHome := u.HomeDir
	area := filepath.Join(realHome, "Zybuu-checkpoints", "hostgit-attacks-r6", "area")
	old := writableAreas
	writableAreas = func() []string { return []string{area} }
	t.Cleanup(func() { writableAreas = old })
	planted := filepath.Join(r.home, "tilde-ran")
	sc := filepath.Join(r.ws, "tilde-ssh")
	os.WriteFile(sc, []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	if err := os.MkdirAll(area, 0o700); err != nil {
		t.Fatal(err)
	}
	inc := filepath.Join(area, "x.gitconfig")
	os.WriteFile(inc, []byte("[core]\n\tsshCommand = "+sc+"\n"), 0o600)
	t.Cleanup(func() { _ = os.Remove(inc); _ = os.Remove(area) })
	rel := strings.TrimPrefix(inc, realHome+"/")
	run(t, r.home, "config", "--global", "include.path", "~"+u.Username+"/"+rel)
	b := r.broker(Network{Open: true})
	_, _, _, err = b.Ask(r.key("c"), r.ws, "git fetch")
	if err == nil || !strings.Contains(err.Error(), "does not follow") {
		t.Errorf("a ~user/ include was not refused: %v", err)
	}
	for _, v := range []string{"%(prefix)/etc/x.gitconfig"} {
		run(t, r.home, "config", "--global", "--replace-all", "include.path", v)
		if _, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c"), r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "does not follow") {
			t.Errorf("%s was not refused: %v", v, err)
		}
	}
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("~user/ INCLUDE IN A WRITABLE AREA STEERED HOST GIT")
	}
}

// D9: relative GIT_CONFIG_GLOBAL: git resolves it from the repository folder.
func TestR6RelativeGitConfigGlobal(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "relg-ran")
	sc := filepath.Join(r.ws, "relg-ssh")
	os.WriteFile(sc, []byte("#!/bin/sh\ntouch "+planted+"\nexit 1\n"), 0o755)
	os.WriteFile(filepath.Join(r.ws, "my.gitconfig"), []byte("[core]\n\tsshCommand = "+sc+"\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", "my.gitconfig")
	b := r.broker(Network{Open: true})
	_, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch")
	if err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Errorf("a relative GIT_CONFIG_GLOBAL was not refused: %v", err)
	}
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	t.Logf("fetch: %v\n%s", err, out)
	if _, err := os.Stat(planted); err == nil {
		t.Errorf("RELATIVE GIT_CONFIG_GLOBAL RESOLVED IN THE WORKSPACE STEERED HOST GIT")
	}
}

// D10: include of a directory, and a chain deeper than git's limit.
func TestR6DirAndDeep(t *testing.T) {
	r := newRemoteRig(t)
	d := filepath.Join(r.home, "incdir")
	os.MkdirAll(d, 0o700)
	run(t, r.home, "config", "--global", "include.path", d)
	b := r.broker(Network{Open: true})
	_, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch")
	if err != nil {
		t.Errorf("an include of a folder (git reads nothing there): %v", err)
	}
	r = newRemoteRig(t)
	prev := filepath.Join(r.home, "inc0")
	run(t, r.home, "config", "--global", "include.path", prev)
	for i := 1; i <= 15; i++ {
		next := filepath.Join(r.home, "inc"+string(rune('a'+i)))
		os.WriteFile(prev, []byte("[include]\n\tpath = "+next+"\n"), 0o600)
		prev = next
	}
	os.WriteFile(prev, []byte("[core]\n\tpager = cat\n"), 0o600)
	b = r.broker(Network{Open: true})
	_, _, _, err = b.Ask(r.key("c2"), r.ws, "git fetch")
	// git stops past its include depth, so the repository's settings cannot be
	// read and the line is left to the sandbox, which has no credentials.
	if err != nil {
		t.Errorf("a 16-deep chain: %v", err)
	}
	if out, _ := runCmd(t, b.Command(r.ctx("c2"), r.ws, "git fetch")); len(r.inner) != 1 {
		t.Errorf("a 16-deep chain was brokered: %q %s", r.inner, out)
	}
	// Past the walk's own limit it refuses, rather than leave the rest unchecked.
	r = newRemoteRig(t)
	for i := range 8 {
		f := filepath.Join(r.home, "wide"+string(rune('a'+i)))
		if err := os.WriteFile(f, []byte("[core]\n\tpager = cat\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, r.home, "config", "--global", "--add", "include.path", f)
	}
	old := maxConfigFiles
	maxConfigFiles = 5
	t.Cleanup(func() { maxConfigFiles = old })
	if _, _, _, err := r.broker(Network{Open: true}).Ask(r.key("c3"), r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "not all of them were checked") {
		t.Errorf("past the limit: %v", err)
	}
}
