package profiles

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProfilesFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", ".abhed", "profiles.json")
	f, err := Load(path)
	if err != nil || len(f.Profiles) != 0 {
		t.Fatalf("missing file = %v %v", f, err)
	}
	f.Profiles["prod"] = &Profile{URL: "https://abhed.test", Token: "abhcli_secret", Username: "ada"}
	f.Current = "prod"
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("profiles mode %o, want 600", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(path))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("profiles dir mode %o, want 700", di.Mode().Perm())
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, p, err := got.Get(""); err != nil || p.Token != "abhcli_secret" {
		t.Fatalf("current profile = %v %v", p, err)
	}
	if _, _, err := got.Get("other"); err == nil {
		t.Error("a missing profile was found")
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Opened up by someone: refused rather than read.
	_ = os.Chmod(path, 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a 0644 profiles file = %v", err)
	}
	_ = os.Chmod(path, 0o600)
	link := filepath.Join(filepath.Dir(path), "link.json")
	_ = os.Symlink(path, link)
	if _, err := Load(link); err == nil {
		t.Error("a profiles file behind a link was read")
	}
	if err := Save(link, got); err == nil {
		t.Error("a profiles file was written through a link")
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://abhed.test/":      "https://abhed.test",
		"https://abhed.test:8443":  "https://abhed.test:8443",
		"http://127.0.0.1:8860":    "http://127.0.0.1:8860",
		"http://localhost:8860/":   "http://localhost:8860",
		"https://abhed.test/team/": "https://abhed.test/team",
	} {
		if got, err := NormalizeURL(in, false); err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"http://abhed.test", "ftp://abhed.test", "abhed.test", "https://u:p@abhed.test", "https://abhed.test/?x=1"} {
		if _, err := NormalizeURL(in, false); err == nil {
			t.Errorf("NormalizeURL(%q) accepted", in)
		}
	}
	if _, err := NormalizeURL("http://abhed.test", true); err != nil {
		t.Errorf("-allow-http refused: %v", err)
	}
	if _, err := Resolve("https://a.test", "http://idp.test/token"); err == nil {
		t.Error("a plain-http endpoint on another host was accepted")
	}
	if got, _ := Resolve("https://a.test", "/v1/cli/token"); got != "https://a.test/v1/cli/token" {
		t.Errorf("relative endpoint = %q", got)
	}
}
