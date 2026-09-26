package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent's tools cannot reach Abhed's own state, whatever the mode: the
// policy that governs the agent must not be writable by the agent.
func TestToolsCannotTouchHarnessState(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(state, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"permissions":{"deny":["bash(*shutdown*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(state, "users.json")

	calls := []struct {
		tool Tool
		args map[string]any
	}{
		{Read{}, map[string]any{"path": cfg}},
		{Write{}, map[string]any{"path": cfg, "content": `{"permissions":{"deny":[]}}`}},
		{Write{}, map[string]any{"path": users, "content": `[{"user":"intruder"}]`}},
		{Edit{}, map[string]any{"path": cfg, "old_string": "shutdown", "new_string": "nothing"}},
		{Glob{}, map[string]any{"pattern": "*.json", "path": state}},
		{Grep{}, map[string]any{"pattern": "deny", "path": state}},
		// A relative spelling and a detour through a sibling both name the same place.
		{Read{}, map[string]any{"path": filepath.Join(dir, "src", "..", StateDir, "config.json")}},
	}
	for _, c := range calls {
		raw, _ := json.Marshal(c.args)
		res := c.tool.Run(context.Background(), s, raw)
		if !res.IsError || !strings.Contains(res.Content, "own state") {
			t.Errorf("%s %s: not refused: %q", c.tool.Name(), c.args["path"], res.Content)
		}
	}
	if got, _ := os.ReadFile(cfg); !strings.Contains(string(got), "shutdown") {
		t.Fatalf("the configuration was changed: %s", got)
	}
	if _, err := os.Stat(users); err == nil {
		t.Fatal("a users file was planted")
	}
}

// On a case-insensitive disk every spelling of the state directory opens the
// same files, and so does a symlink to it; each must be refused.
func TestStateGuardFollowsTheDisk(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(state, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"permissions":{"deny":["bash(*shutdown*)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(state, filepath.Join(dir, "settings")); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "settings", "config.json"), filepath.Join(dir, "settings", "users.json")}
	if _, err := os.Stat(filepath.Join(dir, ".ABHED", "config.json")); err == nil {
		paths = append(paths,
			filepath.Join(dir, ".ABHED", "users.json"),
			filepath.Join(dir, ".Abhed", "config.json"))
	} else {
		t.Log("case-sensitive filesystem: the case variants are checked by name only")
	}
	for _, p := range paths {
		for _, c := range []struct {
			tool Tool
			args map[string]any
		}{
			{Read{}, map[string]any{"path": p}},
			{Write{}, map[string]any{"path": p, "content": `{}`}},
			{Edit{}, map[string]any{"path": p, "old_string": "shutdown", "new_string": "nothing"}},
		} {
			raw, _ := json.Marshal(c.args)
			res := c.tool.Run(context.Background(), s, raw)
			if !res.IsError || !strings.Contains(res.Content, "own state") {
				t.Errorf("%s %s: not refused: %q", c.tool.Name(), p, res.Content)
			}
		}
	}
	for _, name := range []string{".ABHED", ".Abhed"} {
		if !hasStateName(filepath.Join(dir, name, "users.json")) {
			t.Errorf("%s is not recognised as state", name)
		}
	}
	if next, why := detectCd("cd settings", s); next != "" || !strings.Contains(why, "own state") {
		t.Errorf("cd through a symlink to the state: next %q, %q", next, why)
	}
	// A search from the workspace root passes over the state directory.
	for _, c := range []struct {
		tool Tool
		args map[string]any
	}{
		{Grep{}, map[string]any{"pattern": "shutdown", "path": dir, "output_mode": "content"}},
		{Glob{}, map[string]any{"pattern": "**/*.json", "path": dir}},
	} {
		raw, _ := json.Marshal(c.args)
		if res := c.tool.Run(context.Background(), s, raw); strings.Contains(res.Content, "config.json") {
			t.Errorf("%s reached the state: %q", c.tool.Name(), res.Content)
		}
	}
	if got, _ := os.ReadFile(cfg); !strings.Contains(string(got), "shutdown") {
		t.Fatalf("the configuration was changed: %s", got)
	}
}

// A file symlink planted in the workspace reaches whatever it points at, so
// read, write and edit judge the target: a link into the state is refused,
// whether the target exists yet or not.
func TestStateGuardFollowsFileLinks(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(state, "users.json")
	if err := os.WriteFile(users, []byte(`[{"user":"admin","hash":"secret-hash"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"u1": users, "u3": filepath.Join(state, "planted.json")}
	if _, err := os.Stat(filepath.Join(dir, ".ABHED", "users.json")); err == nil {
		links["u2"] = filepath.Join(dir, ".ABHED", "users.json")
	}
	for name, target := range links {
		link := filepath.Join(dir, name)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			tool Tool
			args map[string]any
		}{
			{Read{}, map[string]any{"path": link}},
			{Write{}, map[string]any{"path": link, "content": `[]`}},
			{Edit{}, map[string]any{"path": link, "old_string": "admin", "new_string": "intruder"}},
		} {
			raw, _ := json.Marshal(c.args)
			res := c.tool.Run(context.Background(), s, raw)
			if !res.IsError || !strings.Contains(res.Content, "own state") {
				t.Errorf("%s through %s -> %s: not refused: %q", c.tool.Name(), name, target, res.Content)
			}
		}
	}
	if got, _ := os.ReadFile(users); !strings.Contains(string(got), "secret-hash") {
		t.Fatalf("the users file was changed: %s", got)
	}
	if _, err := os.Lstat(filepath.Join(state, "planted.json")); err == nil {
		t.Fatal("a file was planted in the state through a dangling link")
	}
}

// Glob and grep do not follow links out of bounds, nor read a state file
// under another name.
func TestSearchDoesNotFollowLinksOutOfBounds(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(state, "users.json")
	if err := os.WriteFile(users, []byte("secret-hash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(key, []byte("secret-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("secret-free\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"u1.txt": users, "key.txt": key} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(users, filepath.Join(dir, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pattern": "secret", "path": dir, "output_mode": "content"})
	res := Grep{}.Run(context.Background(), s, raw)
	if strings.Contains(res.Content, "secret-hash") || strings.Contains(res.Content, "secret-key") {
		t.Fatalf("grep read through a link: %q", res.Content)
	}
	if !strings.Contains(res.Content, "secret-free") {
		t.Fatalf("grep missed an ordinary file: %q", res.Content)
	}
	raw, _ = json.Marshal(map[string]any{"pattern": "*.txt", "path": dir})
	res = Glob{}.Run(context.Background(), s, raw)
	for _, name := range []string{"u1.txt", "key.txt", "hard.txt"} {
		if strings.Contains(res.Content, name) {
			t.Errorf("glob listed %s: %q", name, res.Content)
		}
	}
}

// A users or secrets file the configuration keeps outside .abhed is refused
// as .abhed is, by its path and through a link to it.
func TestRegisteredStatePathIsRefused(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	accounts := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(accounts, []byte(`{"admin":"secret-hash"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(accounts, filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	extraState.mu.Lock()
	saved := extraState.paths
	extraState.mu.Unlock()
	t.Cleanup(func() { extraState.mu.Lock(); extraState.paths = saved; extraState.mu.Unlock() })
	AddStatePath(accounts)
	for _, p := range []string{accounts, filepath.Join(dir, "notes.txt")} {
		raw, _ := json.Marshal(map[string]any{"path": p})
		if res := (Read{}).Run(context.Background(), s, raw); !res.IsError || !strings.Contains(res.Content, "own state") {
			t.Errorf("read %s: not refused: %q", p, res.Content)
		}
	}
}

// A folder swapped for a link into the state between the path check and the
// open is refused by read, and write never lands in the state.
func TestSwappedLinkIsNeverFollowed(t *testing.T) {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "users.json"), []byte("secret-hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoyDir, decoyLink, decoy := filepath.Join(dir, "d-dir"), filepath.Join(dir, "d-link"), filepath.Join(dir, "decoy")
	if err := os.MkdirAll(decoyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyDir, "users.json"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(state, decoyLink); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(decoyDir, decoy)
			_ = os.Rename(decoy, decoyDir)
			_ = os.Rename(decoyLink, decoy)
			_ = os.Rename(decoy, decoyLink)
		}
	}()
	target := filepath.Join(decoy, "users.json")
	for i := 0; i < 3000; i++ {
		raw, _ := json.Marshal(map[string]any{"path": target})
		if res := (Read{}).Run(context.Background(), s, raw); strings.Contains(res.Content, "secret-hash") {
			close(stop)
			<-done
			t.Fatalf("read followed the swapped link on try %d", i)
		}
		_ = s.atomicWrite(filepath.Join(decoy, "planted.json"), []byte("x"), 0o600)
	}
	close(stop)
	<-done
	if _, err := os.Stat(filepath.Join(state, "planted.json")); err == nil {
		t.Fatal("a write landed in the state through the swapped link")
	}
}

// The known state files are recognised by identity however many other files
// the state directories hold, so a hardlink to one is refused past the bound.
func TestHardlinkToAKnownStateFileHoldsPastTheBound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, StateDir)
	if err := os.MkdirAll(filepath.Join(state, "a-many"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxStateEntries+10; i++ {
		if err := os.WriteFile(filepath.Join(state, "a-many", fmt.Sprintf("f%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	users := filepath.Join(state, "users.json")
	if err := os.WriteFile(users, []byte("secret-hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(dir, "notes.txt")
	if err := os.Link(users, hard); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"path": hard})
	if res := (Read{}).Run(context.Background(), s, raw); strings.Contains(res.Content, "secret-hash") {
		t.Fatalf("read a hardlink to the users file: %q", res.Content)
	}
}

// A folder swapped for a link to somewhere outside the workspace, between the
// path check and the open, leads neither a read nor a write out of it.
func TestSwappedLinkNeverLeavesTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	s, err := NewSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "notes.txt"), []byte("outside-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	decoyDir, decoyLink, decoy := filepath.Join(dir, "d-dir"), filepath.Join(dir, "d-link"), filepath.Join(dir, "decoy")
	if err := os.MkdirAll(decoyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyDir, "notes.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, decoyLink); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Rename(decoyDir, decoy)
			_ = os.Rename(decoy, decoyDir)
			_ = os.Rename(decoyLink, decoy)
			_ = os.Rename(decoy, decoyLink)
		}
	}()
	for i := 0; i < 3000; i++ {
		raw, _ := json.Marshal(map[string]any{"path": filepath.Join(decoy, "notes.txt")})
		if res := (Read{}).Run(context.Background(), s, raw); strings.Contains(res.Content, "outside-secret") {
			close(stop)
			<-done
			t.Fatalf("read left the workspace through the swapped link on try %d", i)
		}
		_ = s.atomicWrite(filepath.Join(decoy, fmt.Sprintf("planted-%d", i)), []byte("x"), 0o600)
	}
	close(stop)
	<-done
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("a write left the workspace through the swapped link: %d entries outside", len(ents))
	}
}

// A link in the workspace into a directory added with --add-dir is read and
// written in that directory, as a path spelled there would be.
func TestLinkIntoAnAddedDirectoryWorks(t *testing.T) {
	ws, added := t.TempDir(), t.TempDir()
	s, err := NewSession(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddRoot(added); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(added, "lib.go"), []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(added, filepath.Join(ws, "vendored")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "vendored", "lib.go")
	raw, _ := json.Marshal(map[string]any{"path": link})
	if res := (Read{}).Run(context.Background(), s, raw); res.IsError || !strings.Contains(res.Content, "package lib") {
		t.Fatalf("read through a link into an added directory: %q", res.Content)
	}
	raw, _ = json.Marshal(map[string]any{"path": link, "content": "package lib // kept\n"})
	if res := (Write{}).Run(context.Background(), s, raw); res.IsError {
		t.Fatalf("write through a link into an added directory: %q", res.Content)
	}
	if got, _ := os.ReadFile(filepath.Join(added, "lib.go")); !strings.Contains(string(got), "kept") {
		t.Fatalf("the added directory's file was not written: %q", got)
	}
}

// The escape os.Root reports is recognised as ErrOutside; if a Go release
// changes its wording, this fails rather than the refusal going unnamed.
func TestRootEscapeIsErrOutside(t *testing.T) {
	ws, elsewhere := t.TempDir(), t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = r.OpenRoot("out")
	if !errors.Is(outside(err), ErrOutside) {
		t.Fatalf("os.Root's escape error is not recognised: %v", err)
	}
}
