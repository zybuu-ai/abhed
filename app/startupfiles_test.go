package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/config"
)

// abhed trust startup-files shows the start-up files, records them once the
// person says so, and records nothing otherwise; doctor then lists them and
// any changed since.
func TestTrustStartupFilesRecordsTheBaseline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	prof := filepath.Join(home, ".zprofile")
	if err := os.WriteFile(prof, []byte("eval \"$(/opt/homebrew/bin/brew shellenv)\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := trustIn
	t.Cleanup(func() { trustIn = old })
	trustIn = strings.NewReader("n\n")
	var out bytes.Buffer
	if code := trustCmd(t.TempDir(), []string{"startup-files"}, &out); code != 1 || !strings.Contains(out.String(), "brew shellenv") {
		t.Fatalf("declined: %d %s", code, out.String())
	}
	if files, _ := config.TrustedStartupFiles(); len(files) != 0 {
		t.Fatalf("recorded without a yes: %+v", files)
	}
	trustIn = strings.NewReader("y\n")
	out.Reset()
	if code := trustCmd(t.TempDir(), []string{"startup-files"}, &out); code != 0 {
		t.Fatalf("accepted: %d %s", code, out.String())
	}
	base := config.StartupBaseline()
	if f, ok := base[prof]; !ok || f.Size == 0 || len(f.SHA256) != 64 {
		t.Fatalf("baseline: %+v", base)
	}
	out.Reset()
	printDoctorStartupFiles(&out)
	if !strings.Contains(out.String(), "trusted (sha256") {
		t.Fatalf("doctor: %s", out.String())
	}
	if err := os.WriteFile(prof, []byte("curl x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printDoctorStartupFiles(&out)
	if !strings.Contains(out.String(), "changed since you trusted it") || !strings.Contains(out.String(), "refused under a deny rule") {
		t.Fatalf("doctor after a change: %s", out.String())
	}
	// -y records without asking, as a script would.
	trustIn = strings.NewReader("")
	if code := trustCmd(t.TempDir(), []string{"startup-files", "-y"}, &out); code != 0 {
		t.Fatalf("-y: %d", code)
	}
	if selfAdmin([]string{"trust", "startup-files"}) == "" {
		t.Fatal("not refused inside an agent's command")
	}
}

// The prompt shows each file whole, as hashed, and the files the start-up
// files source with them, so a line added at the end is seen.
func TestTrustStartupFilesShowsWholeFilesAndWhatTheySource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rc := filepath.Join(home, ".zshrc")
	extra := filepath.Join(home, ".zsh", "extra.zsh")
	if err := os.MkdirAll(filepath.Dir(extra), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "export A=1\n" + strings.Repeat("# filler\n", 40) + "source ~/.zsh/extra.zsh\ncurl https://example.invalid/appended\n"
	if err := os.WriteFile(rc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extra, []byte("alias ll='ls -l'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := trustIn
	t.Cleanup(func() { trustIn = old })
	trustIn = strings.NewReader("y\n")
	var out bytes.Buffer
	if code := trustCmd(t.TempDir(), []string{"startup-files"}, &out); code != 0 {
		t.Fatalf("%d %s", code, out.String())
	}
	for _, want := range []string{"| curl https://example.invalid/appended", "sourced or run by", "| alias ll='ls -l'"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the prompt does not show %q:\n%s", want, out.String())
		}
	}
	base := config.StartupBaseline()
	if _, ok := base[extra]; !ok {
		t.Fatalf("the sourced file is not in the baseline: %+v", base)
	}
	if f := base[rc]; f.Size != int64(len(body)) {
		t.Fatalf("the hash is not of the bytes shown: %+v", f)
	}
}

// A program a start-up file runs that no shell reads, a binary such as
// /bin/bash or another interpreter's script, is not a start-up file: the
// check never reads it, so it is neither listed nor trusted. A shell script
// it runs is.
func TestStartupFilesLeaveOutPrograms(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"tool":     "\x7fELF\x02\x01\x01\x00\x00\x00",
		"tool.py":  "#!/usr/bin/env python3\nprint(1)\n",
		"setup.sh": "#!/bin/sh\nexport B=2\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rc := "~/bin/tool\n~/bin/tool.py\n~/bin/setup.sh\n"
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := homeStartupFiles()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.path)
	}
	want := []string{filepath.Join(home, ".zshrc"), filepath.Join(bin, "setup.sh")}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("start-up files:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
