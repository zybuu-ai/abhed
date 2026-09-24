package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
)

func (wb *workbench) search(q, flags string) (int, searchResponse) {
	wb.t.Helper()
	rec := wb.get("acme", "search?q="+url.QueryEscape(q)+flags)
	var out searchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func paths(r searchResponse) string {
	var ps []string
	for _, f := range r.Files {
		ps = append(ps, f.Path)
	}
	return strings.Join(ps, " ")
}

// Search reads what the Explorer shows and nothing else: not a read-denied
// file, the server's state, what the grep tool passes over, or binaries.
func TestSearchFindsOnlyWhatTheViewShows(t *testing.T) {
	wb := newWorkbench(t, func(c *config.Config) {
		c.Permissions.Deny = append(c.Permissions.Deny, "read(**/.env)")
	})
	wb.write("src/a.go", "package a\n\nfunc Hello() {}\n")
	wb.write("notes.txt", "say hello\r\nhelloworld\n")
	wb.write(".env", "HELLO=secret\n")
	wb.write(".abhed/config.json", `{"hello":1}`)
	wb.write("node_modules/x/index.js", "hello\n")
	wb.write(".venv/lib/site.py", "hello\n")
	wb.write("blob.bin", "hello\x00\x01")

	code, r := wb.search("hello", "")
	if code != http.StatusOK || paths(r) != "notes.txt src/a.go" || r.Matches != 3 {
		t.Fatalf("search: %d files=%q matches=%d", code, paths(r), r.Matches)
	}
	if got := r.Files[0].Matches[0]; got.Line != 1 || got.Col != 5 || got.End != 10 || got.Text != "say hello" {
		t.Fatalf("match = %+v", got)
	}
	if _, r = wb.search("hello", "&case=1"); paths(r) != "notes.txt" {
		t.Fatalf("match case: %q", paths(r))
	}
	if _, r = wb.search("hello", "&word=1"); r.Matches != 2 {
		t.Fatalf("whole word: %d", r.Matches)
	}
	if _, r = wb.search(`H.l+o\(`, "&regex=1&case=1"); paths(r) != "src/a.go" {
		t.Fatalf("regex: %q", paths(r))
	}
	if code, _ = wb.search("(", "&regex=1"); code != http.StatusBadRequest {
		t.Fatalf("a bad expression answered %d", code)
	}
	if rec := wb.get("other", "search?q=hello"); rec.Code == http.StatusOK {
		t.Fatalf("another tenant searched this workspace: %s", rec.Body)
	}
}

// Columns are counted as the editor counts them, and a long line is cut to a
// window that still holds the match.
func TestSearchColumnsAndPreview(t *testing.T) {
	wb := newWorkbench(t, nil)
	wb.write("u.txt", "é😀 needle\n"+strings.Repeat("x", 1000)+"needle"+strings.Repeat("y", 1000)+"\n")
	_, r := wb.search("needle", "")
	if len(r.Files) != 1 || len(r.Files[0].Matches) != 2 {
		t.Fatalf("search: %+v", r)
	}
	if m := r.Files[0].Matches[0]; m.Col != 5 || m.End != 11 || m.From != 4 || m.To != 10 || m.Text != "é😀 needle" {
		t.Fatalf("utf-16 columns: %+v", m)
	}
	m := r.Files[0].Matches[1]
	if m.Col != 1001 || len(m.Text) > maxSearchPreview || m.Text[m.From:m.To] != "needle" {
		t.Fatalf("long line: col=%d len=%d", m.Col, len(m.Text))
	}
}

// A search is bounded: per file, and in total, and says so.
func TestSearchIsBounded(t *testing.T) {
	wb := newWorkbench(t, nil)
	for i := range 25 {
		wb.write(fmt.Sprintf("f%02d.txt", i), strings.Repeat("hit\n", 150))
	}
	_, r := wb.search("hit", "")
	if len(r.Files[0].Matches) != maxSearchPerFile {
		t.Fatalf("per file: %d", len(r.Files[0].Matches))
	}
	if r.Matches != maxSearchMatches || !r.Truncated || r.Why != "matches" {
		t.Fatalf("total: %d truncated=%v why=%q", r.Matches, r.Truncated, r.Why)
	}
}

// A match longer than the preview window is marked only as far as the window
// goes, so one long line cannot make the reply megabytes long.
func TestSearchPreviewIsBounded(t *testing.T) {
	wb := newWorkbench(t, nil)
	wb.write("min.js", strings.Repeat("z", 1<<20)+"\n")
	_, r := wb.search("z+", "&regex=1")
	m := r.Files[0].Matches[0]
	if len(m.Text) > maxSearchPreview || m.From != 0 || m.To != len(m.Text) || m.End != 1<<20+1 {
		t.Fatalf("preview len=%d from=%d to=%d end=%d", len(m.Text), m.From, m.To, m.End)
	}
}

// A search follows no link out of the workspace.
func TestSearchStaysInsideTheWorkspace(t *testing.T) {
	wb := newWorkbench(t, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wb.write("in.txt", "needle\n")
	for name, target := range map[string]string{"file-link": filepath.Join(outside, "secret.txt"), "dir-link": outside} {
		if err := os.Symlink(target, filepath.Join(wb.workspace, name)); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
	}
	if _, r := wb.search("needle", ""); paths(r) != "in.txt" {
		t.Fatalf("search reached outside the workspace: %q", paths(r))
	}
}

// Each limit that cuts a search short is named in the reply.
func TestSearchSaysWhatCutItShort(t *testing.T) {
	wb := newWorkbench(t, nil)
	wb.write("many.txt", strings.Repeat("hit\n", maxSearchPerFile+1))
	if _, r := wb.search("hit", ""); !r.Files[0].Truncated || len(r.Files[0].Matches) != maxSearchPerFile {
		t.Fatalf("per-file cap not reported: %+v", r.Files[0].Truncated)
	}
	defer func(d time.Duration) { searchDeadline = d }(searchDeadline)
	searchDeadline = 0
	if _, r := wb.search("hit", ""); !r.Truncated || r.Why != "time" {
		t.Fatalf("time limit: truncated=%v why=%q", r.Truncated, r.Why)
	}
}
