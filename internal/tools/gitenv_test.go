package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// A repository's own config cannot make git run an ext:: URL's command: the
// tool's commands get protocol.ext.allow=never, which only the line can lift.
func TestBashGitNeverRunsExtFromConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	s, ws := setup(t)
	marker := filepath.Join(t.TempDir(), "ran")
	if out, err := exec.Command("git", "init", "-q", ws).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	for _, kv := range [][2]string{{"protocol.ext.allow", "always"}, {"remote.o.url", "ext::sh -c touch% " + marker}} {
		if out, err := exec.Command("git", "-C", ws, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	run(t, Bash{}, s, bashArgs{Command: "git fetch o", Description: "fetch"})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran the command an ext:: URL from the repository's config names")
	}
	run(t, Bash{}, s, bashArgs{Command: "git -c protocol.ext.allow=always fetch o", Description: "fetch"})
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("an ext allowance on the line no longer wins:", err)
	}
}

func TestWithGitSafeConfigKeepsEarlierEntries(t *testing.T) {
	env, names := WithGitSafeConfig([]string{"A=1", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=x.y"})
	for _, want := range []string{"A=1", "GIT_CONFIG_KEY_0=x.y", "GIT_CONFIG_COUNT=4", "GIT_CONFIG_KEY_2=protocol.ext.allow",
		"GIT_CONFIG_VALUE_2=never", "GIT_CONFIG_KEY_3=core.fsmonitor", "GIT_CONFIG_VALUE_3=false"} {
		if !slices.Contains(env, want) {
			t.Errorf("env %v lacks %s", env, want)
		}
	}
	if slices.Contains(env, "GIT_CONFIG_COUNT=2") || len(names) != 5 {
		t.Errorf("env %v names %v", env, names)
	}
}

// A repository's core.fsmonitor cannot make git status run a command.
func TestBashGitIgnoresRepositoryFsmonitor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	s, ws := setup(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(t.TempDir(), "mon.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{{"init", "-q", ws}, {"-C", ws, "config", "core.fsmonitor", hook}} {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	run(t, Bash{}, s, bashArgs{Command: "git status", Description: "status"})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git status ran the repository's fsmonitor command")
	}
}
