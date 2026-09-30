package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// answering is a surface whose confirm dialogs get answer; "" is no answer.
type answering struct {
	*ui.LineSurface
	answer  string
	dialogs int
}

func (a *answering) Dialog(context.Context, ui.DialogSpec) (string, error) {
	a.dialogs++
	if a.answer == "" {
		return "", ui.ErrNoAnswer
	}
	return a.answer, nil
}

func configEnv(cfg config.Config, answer string) (*cmdEnv, *answering) {
	s := &answering{LineSurface: ui.NewLineSurface(io.Discard, ui.Style{}, nil), answer: answer}
	return &cmdEnv{ui: s, st: &cliState{appCfg: cfg}}, s
}

// Widening is judged against the person's own file, not the session: a
// session already in bypass, or with a trusted workspace's network on,
// still asks before either is written, and no answer writes nothing.
func TestConfigSetWideningAsksAgainstTheOwnFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, ".abhed", "config.json")
	cfg := config.Default()
	cfg.Permissions.Mode = "bypass"
	cfg.Sandbox.AllowNetwork = true
	for _, c := range [][2]string{{"permissions.mode", "bypass"}, {"sandbox.allow_network", "true"}, {"permissions.mode", "auto"}} {
		e, s := configEnv(cfg, "")
		if err := configSet(context.Background(), e, c[0], c[1]); err == nil || s.dialogs != 1 {
			t.Fatalf("%s %s: err %v, %d dialogs", c[0], c[1], err, s.dialogs)
		}
		if _, err := os.Stat(file); err == nil {
			t.Fatalf("%s %s was written with no answer", c[0], c[1])
		}
	}
	e, s := configEnv(cfg, ui.ChoiceYes)
	if err := configSet(context.Background(), e, "permissions.mode", "auto"); err != nil || s.dialogs != 1 {
		t.Fatalf("after a yes: %v, %d dialogs", err, s.dialogs)
	}
	// The file now says auto: plan narrows it, so no question; bypass widens it.
	e, s = configEnv(config.Default(), "")
	if err := configSet(context.Background(), e, "permissions.mode", "plan"); err != nil || s.dialogs != 0 {
		t.Fatalf("narrowing: %v, %d dialogs", err, s.dialogs)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), `"plan"`) {
		t.Fatalf("%s", data)
	}
	// A file that already says so is not widened by saying it again.
	if err := os.WriteFile(file, []byte(`{"sandbox":{"allow_network":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e, s = configEnv(config.Default(), "")
	if err := configSet(context.Background(), e, "sandbox.allow_network", "true"); err != nil || s.dialogs != 0 {
		t.Fatalf("unchanged: %v, %d dialogs", err, s.dialogs)
	}
}

// The file is replaced through a temporary file of its own, never a
// predictable name a link could stand in for.
func TestWriteUserSettingIgnoresAPlantedTmp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".abhed")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "config.json.tmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeUserSetting("limits.max_turns", 5); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep" {
		t.Fatalf("the link was followed: %q", data)
	}
}
