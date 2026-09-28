package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// foldsCase reports whether dir's disk opens a name in another case. The
// default APFS on macOS and NTFS do; most Linux disks do not.
func foldsCase(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(filepath.Join(dir, "probe"))
	_, err := os.Stat(filepath.Join(dir, "PROBE"))
	return err == nil
}

// On a disk that folds case, core/VAULT opens core/vault, so a deny or ask
// rule on the folder holds for every spelling of it, for every file tool.
func TestPathRulesHoldForEveryCaseTheDiskOpens(t *testing.T) {
	ws := t.TempDir()
	if !foldsCase(t, ws) {
		t.Skip("this disk keeps case, so another spelling is another path")
	}
	for _, d := range []string{"core/vault/keys", "ops/frozen", "docs/archive"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e := New(ModeBypass)
	e.Roots = func() []string { return []string{ws} }
	if err := e.AddDeny("write(**/vault/**)", "edit(**/vault/**)", "delete(**/vault/**)", "write(ops/frozen/**)", "mkdir(**/frozen/**)"); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAsk("delete(**/archive/**)"); err != nil {
		t.Fatal(err)
	}
	path := func(p string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"path": p})
		return b
	}
	for _, c := range []struct {
		tool, path string
		want       Decision
	}{
		{"write", "core/VAULT/keys/master.txt", Deny},
		{"edit", "Core/Vault/KEYS/master.txt", Deny},
		{"delete", "core/VAULT/", Deny},
		{"write", "OPS/Frozen/new.txt", Deny},      // a relative rule, a new file
		{"mkdir", "ops/FROZEN/newdir/", Deny},      // a folder not made yet, under one that is
		{"delete", "DOCS/ARCHIVE/2025/q4.md", Ask}, // an ask rule too
		{"write", "core/VAULTED/other.txt", Allow}, // another name is not the folder
		{"write", "core/Vault2/other.txt", Allow},
	} {
		p := filepath.Join(ws, c.path)
		if strings.HasSuffix(c.path, "/") {
			p += string(filepath.Separator) // a folder, as the Explorer names one
		}
		if d := e.Evaluate(c.tool, true, path(p)); d.Decision != c.want {
			t.Errorf("%s %s: %s (%s), want %s", c.tool, c.path, d.Decision, d.Reason, c.want)
		}
	}
}

// A workspace named in another case than the disk holds, as a typed cd leaves it,
// still has its relative rules matched against the disk's spelling of a path.
func TestRelativeRulesHoldUnderARootNamedInAnotherCase(t *testing.T) {
	parent := t.TempDir()
	if !foldsCase(t, parent) {
		t.Skip("this disk keeps case, so another spelling is another path")
	}
	ws := filepath.Join(parent, "Proj")
	if err := os.MkdirAll(filepath.Join(ws, "docs", "frozen"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := New(ModeBypass)
	e.Roots = func() []string { return []string{filepath.Join(parent, "PROJ")} }
	if err := e.AddDeny("write(docs/frozen/**)"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(parent, "PROJ", "docs", "FROZEN", "x.md"),
		filepath.Join(ws, "Docs", "Frozen", "x.md"),
	} {
		args, _ := json.Marshal(map[string]string{"path": p})
		if d := e.Evaluate("write", true, args); d.Decision != Deny {
			t.Errorf("%s: %s, want deny", p, d.Decision)
		}
	}
}
