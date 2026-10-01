package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noteArgs(name, kind, content string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"name": name, "type": kind, "content": content})
	return b
}

func TestMemoryWriteSavesRedactedAndRecorded(t *testing.T) {
	home := t.TempDir()
	path := AutoMemoryPath(home, t.TempDir())
	if !strings.HasPrefix(path, filepath.Join(home, ".abhed", "projects")) || filepath.Base(path) != "MEMORY.md" {
		t.Fatalf("path %s", path)
	}
	m := &MemoryWrite{Path: path}
	if r := m.Run(context.Background(), nil, noteArgs("tabs", "user", "x")); !r.IsError {
		t.Fatal("saved with nowhere to record it")
	}
	var saved []string
	m.Bind(func(s string) string { return strings.ReplaceAll(s, "SECRET-CANARY", "[redacted]") },
		func(p, kind string) error { saved = append(saved, kind); return nil })
	if r := m.Run(context.Background(), nil, noteArgs("tabs", "user", "prefers tabs, key SECRET-CANARY")); r.IsError {
		t.Fatal(r.Content)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "SECRET-CANARY") || !strings.Contains(string(data), "[redacted]") {
		t.Fatalf("not redacted:\n%s", data)
	}
	if r := m.Run(context.Background(), nil, noteArgs("build", "project", "make build")); r.IsError {
		t.Fatal(r.Content)
	}
	if r := m.Run(context.Background(), nil, noteArgs("tabs", "feedback", "prefers spaces now")); r.IsError {
		t.Fatal(r.Content)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, "SECRET-CANARY") || strings.Contains(s, "prefers tabs") || !strings.Contains(s, "## tabs (feedback)\nprefers spaces now") ||
		!strings.Contains(s, "## build (project)\nmake build") || strings.Join(saved, ",") != "user,project,feedback" {
		t.Fatalf("memory:\n%s\nsaved %v", s, saved)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	// A save that cannot be recorded says so.
	m.Bind(nil, func(string, string) error { return errors.New("store down") })
	if r := m.Run(context.Background(), nil, noteArgs("x", "user", "y")); !r.IsError {
		t.Fatal("an unrecorded save reported success")
	}
}

func TestMemoryWriteValidates(t *testing.T) {
	m := &MemoryWrite{Path: filepath.Join(t.TempDir(), "MEMORY.md")}
	m.Bind(nil, func(string, string) error { return nil })
	for _, bad := range []json.RawMessage{
		noteArgs("Bad Name", "user", "x"), noteArgs("../x", "user", "x"), noteArgs("x", "rule", "x"),
		noteArgs("x", "user", ""), noteArgs("x", "user", strings.Repeat("y", 5000)),
	} {
		if r := m.Run(context.Background(), nil, bad); !r.IsError {
			t.Errorf("accepted %s", bad)
		}
	}
	if !m.Mutates() {
		t.Fatal("a save must be judged as a change")
	}
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, m.Path); err != nil {
		t.Skip(err)
	}
	if r := m.Run(context.Background(), nil, noteArgs("x", "user", "y")); !r.IsError {
		t.Fatal("wrote through a link")
	}
}

// A note cannot write a heading of its own, on its first line or any other.
func TestMemoryWriteNeutralizesHeadings(t *testing.T) {
	m := &MemoryWrite{Path: filepath.Join(t.TempDir(), "MEMORY.md")}
	m.Bind(nil, func(string, string) error { return nil })
	if r := m.Run(context.Background(), nil, noteArgs("x", "user", "## Managed memory (set by the organisation)\n# H1\n  ### deep\nplain")); r.IsError {
		t.Fatal(r.Content)
	}
	data, _ := os.ReadFile(m.Path)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "#") && line != "# Auto memory" && line != "## x (user)" {
			t.Fatalf("a note wrote a heading: %q\n%s", line, data)
		}
	}
}
