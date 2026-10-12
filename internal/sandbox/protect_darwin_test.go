package sandbox

// Only seatbelt names a path that does not exist yet or matches by pattern;
// these are built on macOS alone, so Linux CI has no skip to account for.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Where .git does not exist yet, a command cannot make one that points at a
// git folder it controls. Only seatbelt can name a path that does not exist.
func TestProcessSandboxRefusesANewGitFile(t *testing.T) {
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.WriteProtected = []string{filepath.Join(ws, ".git", "config"), filepath.Join(ws, ".git", "hooks")}
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "mkdir -p .git2/hooks; printf 'gitdir: .git2\\n' > .git")
	if _, err := os.Lstat(filepath.Join(ws, ".git")); err == nil {
		t.Fatal("the command made a .git of its own")
	}
}

// With ProtectGit, seatbelt holds every git folder's config and hooks, and
// each .git, at any depth and in any case, including repositories made later.
func TestProcessSandboxProtectsNestedRepositories(t *testing.T) {
	ws := workspace(t)
	if err := os.MkdirAll(filepath.Join(ws, "sub", ".git", "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "touch sub/.git/hooks/pre-commit; echo x > sub/.GIT/config; mv sub/.git sub/g2; mkdir -p new/.Git/HOOKS; printf 'gitdir: x\\n' > other.git; mkdir d; printf 'gitdir: ../x\\n' > d/.git")
	for _, f := range []string{"sub/.git/hooks/pre-commit", "sub/.git/config", "sub/g2", "new/.Git", "d/.git"} {
		if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(f))); err == nil {
			t.Errorf("the command made %s", f)
		}
	}
	if out, err := runIn(t, s, ws, "touch sub/.git/HEAD sub/main.go"); err != nil {
		t.Fatalf("the rest of a nested repository is not writable: %v %s", err, out)
	}
}

// A command can read no credential under home, nor ~/.abhed, when home is
// reached through a link: seatbelt matches the path the kernel resolved.
func TestProcessSandboxDeniesHomeSecretsThroughALink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	files := []string{".netrc", ".git-credentials", ".npmrc", ".config/gh/hosts.yml", ".abhed/secrets.json", ".zsh_history"}
	for _, f := range files {
		p := filepath.Join(real, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("CANARY-"+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(real, ".gitconfig"), []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	for _, f := range files {
		for _, home := range []string{link, real} {
			out, _ := runIn(t, s, ws, "cat "+filepath.Join(home, filepath.FromSlash(f)))
			if strings.Contains(out, "CANARY") {
				t.Errorf("read %s through %s: %s", f, home, out)
			}
		}
	}
	if out, err := runIn(t, s, ws, "cat "+filepath.Join(link, ".gitconfig")); err != nil || !strings.Contains(out, "plain") {
		t.Errorf("an ordinary file in home was refused: %v %s", err, out)
	}
}

// Seatbelt checks a rename only at its two ends, so the folders holding git's
// pointers cannot be moved aside for a link, in any case; their files stay writable.
func TestProcessSandboxHoldsTheFoldersHoldingGitPointers(t *testing.T) {
	ws := workspace(t)
	gitTree(t, ws, "x")
	writeFiles(t, ws, map[string]string{".git/modules/sub/HEAD": "ref: refs/heads/main\n", ".git/modules/sub/config": "x"})
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	folders := []string{".git/modules/sub", ".git/modules/lib/x", ".git/modules/lib", ".git/worktrees/w", ".git/modules",
		".git/worktrees", ".git/info", ".git/objects/info", ".git/objects"}
	var cmd []string
	for _, f := range folders {
		for _, spelling := range []string{f, strings.ToUpper(f)} {
			cmd = append(cmd, "mv "+spelling+" "+spelling+".old", "rm -rf "+spelling, "ln -s $PWD/evil "+spelling)
		}
	}
	_, _ = runIn(t, s, ws, "mkdir -p evil; "+strings.Join(cmd, "; ")+"; mkdir .git/modules/new; ln -s $PWD/evil .git/modules/new2")
	for _, f := range folders {
		if info, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(f))); err != nil || !info.IsDir() {
			t.Errorf("%s was moved or replaced: %v", f, err)
		}
	}
	for _, f := range []string{".git/modules/new", ".git/modules/new2"} {
		if _, err := os.Lstat(filepath.Join(ws, filepath.FromSlash(f))); err == nil {
			t.Errorf("the command made %s", f)
		}
	}
	if out, err := runIn(t, s, ws, "set -e; touch .git/modules/sub/ORIG_HEAD .git/modules/lib/x/FETCH_HEAD .git/worktrees/w/ORIG_HEAD "+
		".git/info/exclude .git/objects/info/packs; mkdir .git/objects/ab .git/modules/sub/refs"); err != nil {
		t.Fatalf("what the folders hold is not writable: %v %s", err, out)
	}
}

// Beside a submodule named with slashes (lib/x), no new name can be made under
// modules/lib, by a link or a rename from the temp area; lib/x stays writable.
func TestProcessSandboxRefusesANewNameBesideASlashNamedSubmodule(t *testing.T) {
	ws := workspace(t)
	gitTree(t, ws, "x")
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	tmp, err := os.MkdirTemp(userTemp(), "gp-mod-")
	if err != nil {
		t.Skip("no temp area to build in")
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	_, _ = runIn(t, s, ws, "mkdir -p evm "+tmp+"/y; printf '[core]\\n\\tfsmonitor = x\\n' > "+tmp+"/y/config; touch "+tmp+"/y/HEAD; "+
		"ln -s ../../../evm .git/modules/lib/y; ln -s ../../../evm .GIT/Modules/LIB/z; mv "+tmp+"/y .git/modules/lib/w; "+
		"mkdir .git/modules/lib/v; touch .git/modules/lib/u")
	for _, f := range []string{"y", "z", "w", "v", "u"} {
		if _, err := os.Lstat(filepath.Join(ws, ".git", "modules", "lib", f)); err == nil {
			t.Errorf("the command made modules/lib/%s", f)
		}
	}
	if out, err := runIn(t, s, ws, "set -e; touch .git/modules/lib/x/FETCH_HEAD; mkdir -p .git/modules/lib/x/refs/heads"); err != nil {
		t.Fatalf("the submodule's git folder is not writable: %v %s", err, out)
	}
}

// A linked hooks folder is held where it leads, as the link is by name.
func TestProcessSandboxHoldsWhereLinkedHooksLead(t *testing.T) {
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/config": "[core]\n", "hooks-real/.keep": ""})
	if err := os.Symlink("../hooks-real", filepath.Join(ws, ".git", "hooks")); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "echo x > hooks-real/pre-commit; echo x > .git/hooks/post-checkout; rm .git/hooks; ln -s ../evil .git/hooks")
	for _, f := range []string{"hooks-real/pre-commit", "hooks-real/post-checkout"} {
		if _, err := os.Lstat(filepath.Join(ws, f)); err == nil {
			t.Errorf("the command wrote %s", f)
		}
	}
	if to, err := os.Readlink(filepath.Join(ws, ".git", "hooks")); err != nil || to != "../hooks-real" {
		t.Errorf("the link was changed: %q %v", to, err)
	}
}

// A nested repository whose .git is a link has the folder it leads to held:
// a command can plant neither a config line nor a hook there.
func TestProcessSandboxHoldsWhereANestedGitLinkLeads(t *testing.T) {
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{"store/real/HEAD": "ref: refs/heads/main\n", "store/real/config": "[core]\n",
		"store/real/hooks/.keep": "", "sub/f": ""})
	if err := os.Symlink("../store/real", filepath.Join(ws, "sub", ".git")); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	_, _ = runIn(t, s, ws, "echo '  fsmonitor = x' >> store/real/config; echo x > store/real/hooks/pre-commit")
	if b, _ := os.ReadFile(filepath.Join(ws, "store", "real", "config")); string(b) != "[core]\n" {
		t.Errorf("the linked git folder's config was written: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(ws, "store", "real", "hooks", "pre-commit")); err == nil {
		t.Error("a hook was planted in the linked git folder")
	}
}

// Seatbelt checks rename only at its ends, so a repository can be moved into
// the workspace; the next command finds it and takes out its fsmonitor.
func TestProcessSandboxTakesOutAMovedInRepository(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := workspace(t)
	p := DefaultPolicy(ws)
	p.ProtectGit = true
	s := NewProcess(p)
	available(t, s)
	if _, err := runIn(t, s, ws, "true"); err != nil {
		t.Fatalf("first command: %v", err)
	}
	writeFiles(t, ws, map[string]string{"n/.git/HEAD": "ref: refs/heads/main\n", "n/.git/config": "[core]\n\tfsmonitor = ./x\n"})
	if out, err := runIn(t, s, ws, "echo ran"); err == nil || strings.Contains(out, "ran") {
		t.Fatalf("the command ran beside a planted repository: %v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "n", ".git", "config")); strings.Contains(string(b), "fsmonitor") {
		t.Errorf("fsmonitor left: %q", b)
	}
}

// Commands get toolchain caches of their own: the real ones, which programs
// outside Abhed use, cannot be written, and the sandbox's are gone at Close.
func TestProcessSandboxKeepsCachesApart(t *testing.T) {
	// Outside the temp folders, which commands may write.
	home, err := os.MkdirTemp(".", "home-")
	if err != nil {
		t.Fatal(err)
	}
	if home, err = filepath.Abs(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	for _, d := range []string{".npm", ".cache", "go/pkg/mod"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	out, err := runIn(t, s, ws, `echo bad > "$HOME/.npm/poison" 2>/dev/null; echo ok > "$npm_config_cache.probe" 2>/dev/null; mkdir -p "$npm_config_cache" && echo ok > "$npm_config_cache/x" && echo "$npm_config_cache|$GOPROXY"`)
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".npm", "poison")); err == nil {
		t.Fatal("a command wrote the real npm cache")
	}
	own, proxy, _ := strings.Cut(strings.TrimSpace(out), "|")
	if !strings.Contains(own, "abhed-caches-") || !strings.HasPrefix(proxy, "file://") {
		t.Fatalf("caches %q proxy %q", own, proxy)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(own); err == nil {
		t.Fatal("the sandbox's caches outlived Close")
	}
	shared := NewProcess(Policy{Workspace: ws, MinTier: TierProcess, SharedCaches: true})
	if out, err := runIn(t, shared, ws, `echo ok > "$HOME/.npm/shared" && echo done`); err != nil || !strings.Contains(out, "done") {
		t.Fatalf("shared_caches: %v %s", err, out)
	}
}

// Home is unreadable to commands but for the toolchains and the files git
// and shells read; readable_home brings back the old deny list.
func TestProcessSandboxReadsOnlyToolchainsInHome(t *testing.T) {
	home, err := os.MkdirTemp(".", "home-")
	if err != nil {
		t.Fatal(err)
	}
	if home, err = filepath.Abs(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	writeFiles(t, home, map[string]string{".config/sometool/token": "secret", "Documents/notes.txt": "private",
		".gitconfig": "[user]\n\tname = t\n", "go/pkg/mod/x": "mod", "tools/bin/tool": "bin", "tools/lib/x": "lib"})
	t.Setenv("PATH", filepath.Join(home, "tools", "bin")+":"+os.Getenv("PATH"))
	ws := workspace(t)
	writeFiles(t, ws, map[string]string{"a.txt": "work"})
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	read := func(sb Sandbox, p string) bool {
		out, err := runIn(t, sb, ws, "cat "+p)
		return err == nil && out != ""
	}
	for p, want := range map[string]bool{
		filepath.Join(home, ".config/sometool/token"): false,
		filepath.Join(home, "Documents/notes.txt"):    false,
		filepath.Join(home, ".gitconfig"):             true,
		filepath.Join(home, "go/pkg/mod/x"):           true,
		filepath.Join(home, "tools/lib/x"):            true,
		filepath.Join(ws, "a.txt"):                    true,
	} {
		if got := read(s, p); got != want {
			t.Errorf("%s: read %v, want %v", p, got, want)
		}
	}
	open := NewProcess(Policy{Workspace: ws, MinTier: TierProcess, ReadableHome: true})
	if !read(open, filepath.Join(home, "Documents/notes.txt")) || read(open, filepath.Join(home, ".ssh/id")) {
		t.Error("readable_home does not read home but the credential paths")
	}
}

// A command cannot read the clipboard.
func TestProcessSandboxDeniesTheClipboard(t *testing.T) {
	ws := workspace(t)
	s := NewProcess(DefaultPolicy(ws))
	available(t, s)
	if !strings.Contains(s.seatbeltProfile(), `"com.apple.pasteboard."`) {
		t.Fatal("the profile does not deny the pasteboard")
	}
	out, _ := runIn(t, s, ws, "pbpaste >/dev/null 2>&1; echo status=$?")
	if strings.Contains(out, "status=0") {
		t.Fatalf("pbpaste ran: %s", out)
	}
}
