package mountns

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// aliasCaseEnv names the case a child started by these tests runs, in a user
// and mount namespace of its own (Attr), where it can make bind mounts.
const aliasCaseEnv = "ABHED_TEST_MOUNTNS_ALIAS"

func init() {
	c := os.Getenv(aliasCaseEnv)
	if c == "" {
		return
	}
	if err := aliasCase(c, os.Getenv("ABHED_TEST_MOUNTNS_DIR")); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("ok")
	os.Exit(0)
}

// aliasWorkspace makes a workspace under dir with git's config and hooks
// and Abhed's state, and the plan that holds them.
func aliasWorkspace(dir string) (Plan, error) {
	ws := filepath.Join(dir, "ws")
	for _, d := range []string{".git/hooks", ".abhed", "src"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o700); err != nil {
			return Plan{}, err
		}
	}
	for name, body := range map[string]string{".git/config": "[core]\n", ".git/hooks/pre-commit": "#!/bin/sh\n", ".abhed/config.json": "abhed-state", "notes.txt": ""} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o600); err != nil {
			return Plan{}, err
		}
	}
	return Plan{Root: ws, Pin: []string{".git"}, ReadOnly: []string{".git/config", ".git/hooks"}, Empty: []string{".abhed"}}, nil
}

func bind(from, to string, dir bool) error {
	var err error
	if dir {
		err = os.MkdirAll(to, 0o700)
	} else {
		err = os.WriteFile(to, nil, 0o600)
	}
	if err != nil {
		return err
	}
	return unix.Mount(from, to, "", unix.MS_BIND, "")
}

func aliasCase(name, dir string) error {
	p, err := aliasWorkspace(dir)
	if err != nil {
		return err
	}
	ws := p.Root
	switch name {
	case "covered":
		// The whole workspace, and its .git, .abhed and git config alone,
		// each mounted a second time.
		for _, b := range []struct {
			from, to string
			dir      bool
		}{{ws, "alias-ws", true}, {filepath.Join(ws, ".git"), "alias-git", true}, {filepath.Join(ws, ".abhed"), "alias-state", true},
			{filepath.Join(ws, ".git", "config"), "alias-config", false}, {filepath.Join(ws, "src"), "alias-src", true}} {
			if err := bind(b.from, filepath.Join(dir, b.to), b.dir); err != nil {
				return fmt.Errorf("binding %s: %w", b.to, err)
			}
		}
		got, _, err := Aliases(ws)
		if err != nil {
			return err
		}
		// A host may mount the folder elsewhere too, as an ostree host does.
		for _, b := range []string{"alias-ws", "alias-git", "alias-state", "alias-config", "alias-src"} {
			if !slices.Contains(got, filepath.Join(dir, b)) {
				return fmt.Errorf("aliases: %v, without %s", got, b)
			}
		}
		if err := Apply(p); err != nil {
			return err
		}
		if err := Drop(); err != nil {
			return err
		}
		for _, w := range []string{"alias-ws/.git/config", "alias-ws/.git/hooks/pre-commit", "alias-ws/.git/hooks/x", "alias-git/config", "alias-git/hooks/x", "alias-config"} {
			f, err := os.OpenFile(filepath.Join(dir, w), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
			if err == nil {
				_ = f.Close()
				return fmt.Errorf("%s was opened for writing through an alias", w)
			}
			if !errors.Is(err, unix.EROFS) {
				return fmt.Errorf("%s: not refused as read-only: %w", w, err)
			}
		}
		for _, r := range []string{"alias-ws/.abhed/config.json", "alias-state/config.json"} {
			if b, err := os.ReadFile(filepath.Join(dir, r)); err == nil {
				return fmt.Errorf("%s was read through an alias: %q", r, b)
			}
		}
		// What the plan does not hold stays writable through its alias.
		return os.WriteFile(filepath.Join(dir, "alias-src", "x"), nil, 0o600)
	case "shadowed":
		// A second mount of the workspace whose path now reaches something
		// else cannot be checked, so the plan is refused.
		if err := bind(ws, filepath.Join(dir, "alias"), true); err != nil {
			return err
		}
		if err := unix.Mount("tmpfs", filepath.Join(dir, "alias"), "tmpfs", 0, ""); err != nil {
			return err
		}
		if err := Apply(p); err == nil || !errors.Is(err, errAlias) {
			return fmt.Errorf("a shadowed alias: %w", err)
		}
		return nil
	case "hardlink":
		// A hook named also outside the hooks folder is refused.
		if err := os.Remove(filepath.Join(ws, "notes.txt")); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(ws, ".git", "hooks", "pre-commit"), filepath.Join(ws, "notes.txt")); err != nil {
			return err
		}
		if err := Apply(p); err == nil || !strings.Contains(err.Error(), "hard link") {
			return fmt.Errorf("a hook with a second name outside the hooks folder: %w", err)
		}
		return nil
	case "ownbarrier":
		// An alias behind a folder of the user's own is refused: a command
		// could chmod that folder and reach it.
		own := filepath.Join(dir, "own")
		if err := bind(ws, filepath.Join(own, "alias"), true); err != nil {
			return err
		}
		if err := os.Chmod(own, 0); err != nil {
			return err
		}
		defer func() { _ = os.Chmod(own, 0o700) }()
		if err := Apply(p); !errors.Is(err, errAlias) {
			return fmt.Errorf("an alias behind the user's own folder: %w", err)
		}
		return nil
	case "barred":
		// An alias behind another user's folder the user cannot search, here
		// /root bound over its parent, is left and listed as unreachable.
		alias := filepath.Join(dir, "b", "alias")
		if err := bind(ws, alias, true); err != nil {
			return err
		}
		if err := unix.Mount("/root", filepath.Join(dir, "b"), "", unix.MS_BIND, ""); err != nil {
			return err
		}
		got, unreachable, err := Aliases(ws)
		if err != nil {
			return err
		}
		if slices.Contains(got, alias) || !slices.Contains(unreachable, alias) {
			return fmt.Errorf("aliases %v, unreachable %v", got, unreachable)
		}
		if err := Apply(p); err != nil {
			return err
		}
		if err := Drop(); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(alias, ".abhed", "config.json")); !errors.Is(err, unix.EACCES) {
			return fmt.Errorf("the unreachable alias: %w", err)
		}
		return nil
	case "linkinside":
		// Two names inside the held folder are both read-only, and pass.
		if err := os.Link(filepath.Join(ws, ".git", "hooks", "pre-commit"), filepath.Join(ws, ".git", "hooks", "pre-push")); err != nil {
			return err
		}
		return Apply(p)
	}
	return fmt.Errorf("no case %q", name)
}

// Each case runs in a child holding CAP_SYS_ADMIN in a user namespace of its
// own, where the bind mounts it makes stand for a host's: an ostree host's
// /sysroot is the same case as alias-ws.
func TestApplyCoversAliases(t *testing.T) {
	if os.Getenv("ABHED_REQUIRE_FENCE") != "1" {
		t.Skip("set ABHED_REQUIRE_FENCE=1 where an ordinary user can make a user namespace")
	}
	for _, c := range []string{"covered", "shadowed", "hardlink", "linkinside", "ownbarrier", "barred"} {
		t.Run(c, func(t *testing.T) {
			if c == "barred" && unix.Access("/root", unix.X_OK) == nil {
				t.Skip("/root is searchable here, so it cannot bar an alias")
			}
			cmd := exec.Command("/proc/self/exe", "-test.run=^$")
			cmd.Env = append(os.Environ(), aliasCaseEnv+"="+c, "ABHED_TEST_MOUNTNS_DIR="+t.TempDir())
			cmd.SysProcAttr = &syscall.SysProcAttr{}
			Attr(cmd.SysProcAttr)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "ok" {
				t.Fatalf("%v\n%s", err, out)
			}
		})
	}
}
