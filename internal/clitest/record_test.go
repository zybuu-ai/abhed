package clitest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

func TestReadRecordLinks(t *testing.T) {
	dir := t.TempDir()
	good := `{"seq":1,"type":"session.started","actor":"system","payload":{},"prev":"","hash":"h1"}
{"seq":2,"type":"mode.changed","actor":"user","payload":{"from":"default","to":"plan","via":"shift-tab","by":"user"},"prev":"h1","hash":"h2"}
`
	path := filepath.Join(dir, "s-1.jsonl")
	writeRec(t, path, good)
	r, err := ReadRecordFile(path)
	if err != nil || !r.Verified || len(r.Events) != 2 {
		t.Fatalf("%v %+v", err, r)
	}
	if got := RecordGolden(r.Events); got != "session.started · system\nmode.changed · user · by:user from:default to:plan via:shift-tab\n" {
		t.Fatalf("golden %q", got)
	}
	for name, body := range map[string]string{
		"broken link":  strings.Replace(good, `"prev":"h1"`, `"prev":"hX"`, 1),
		"skipped seq":  strings.Replace(good, `"seq":2`, `"seq":3`, 1),
		"missing hash": strings.Replace(good, `"hash":"h2"`, `"hash":""`, 1),
	} {
		writeRec(t, path, body)
		if r, _ := ReadRecordFile(path); r.Verified {
			t.Errorf("%s verified", name)
		}
	}
	writeRec(t, path, good)
	writeRec(t, filepath.Join(dir, "head", "s-1"), "1 h1\n")
	if r, _ := ReadRecordFile(path); r.Verified {
		t.Error("a head file naming another line verified")
	}
}

func TestReadRecordNone(t *testing.T) {
	if _, err := readRecord(t.TempDir()); err != errNoRecord {
		t.Fatalf("%v", err)
	}
}

func TestParseEvents(t *testing.T) {
	evs := ParseEvents("noise\n{\"type\":\"session.started\"}\n{\"not\":1}\n")
	if len(evs) != 1 || evs[0].Type != agent.EvSessionStarted {
		t.Fatalf("%+v", evs)
	}
}

func writeRec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
