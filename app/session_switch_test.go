package app

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
	"github.com/zybuu-ai/abhed/store"
)

// listingStore is a database-like store: session rows to list and get.
type listingStore struct {
	*agent.MemStore
	rows []store.SessionRecord
}

func (l *listingStore) ListSessions(context.Context, int) ([]store.SessionRecord, error) {
	return append([]store.SessionRecord(nil), l.rows...), nil
}

func (l *listingStore) GetSession(_ context.Context, id string) (store.SessionRecord, error) {
	for _, r := range l.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return store.SessionRecord{}, store.ErrNotFound
}

// picking is a surface that records the pick it is shown and answers it.
type picking struct {
	*ui.LineSurface
	answer string
	spec   ui.PickSpec
}

func (p *picking) Pick(_ context.Context, spec ui.PickSpec) (string, error) {
	p.spec = spec
	return p.answer, nil
}

func dbRows(now time.Time) []store.SessionRecord {
	ended := now.Add(-time.Hour)
	return []store.SessionRecord{
		{ID: "s-old", Tenant: "default", User: "ana", Prompt: "terraform cleanup", StartedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-time.Minute), EndedAt: &ended, TerminalReason: "completed"},
		{ID: "s-new", Tenant: "default", User: "ana", Title: "Release notes", Prompt: "draft the notes", StartedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "s-bo", Tenant: "default", User: "bo", Prompt: "someone else's", StartedAt: now, UpdatedAt: now},
		{ID: "s-sub", Tenant: "default", User: "ana", ParentID: "s-old", Prompt: "a subagent", StartedAt: now, UpdatedAt: now},
	}
}

// With the record in a database, the picker lists this user's sessions by
// title or first prompt, latest active first, filters, and marks the one open.
func TestStoredPickerListsTitlesAndMarksCurrent(t *testing.T) {
	t.Setenv("USER", "ana")
	surface := &picking{LineSurface: ui.NewLineSurface(io.Discard, ui.Style{}, nil), answer: "s-old"}
	st := &cliState{appCfg: config.Default(), store: &listingStore{MemStore: agent.NewMemStore(), rows: dbRows(time.Now())},
		surface: surface, sessionID: "s-new"}
	var said []string
	say := func(f string, a ...any) { said = append(said, f) }

	id, err := pickSession(context.Background(), st, say)
	if err != nil || id != "s-old" {
		t.Fatalf("picked %q, %v", id, err)
	}
	items := surface.spec.Items
	if len(items) != 2 || items[0].ID != "s-old" || items[1].ID != "s-new" {
		t.Fatalf("the picker offered %+v; want this user's two, latest active first", items)
	}
	if items[0].Label != "terraform cleanup" || items[1].Label != "Release notes" {
		t.Errorf("labels are %q and %q; want the first prompt, then the title", items[0].Label, items[1].Label)
	}
	if !strings.HasPrefix(items[1].Detail, "● current") || strings.Contains(items[0].Detail, "current") {
		t.Errorf("the current session is not the one marked: %q / %q", items[0].Detail, items[1].Detail)
	}
	if !surface.spec.Filter || surface.spec.Default != "s-old" {
		t.Errorf("filter %v, default %q; want a filtering pick that does not default to the open session", surface.spec.Filter, surface.spec.Default)
	}

	surface.answer = "s-new"
	if id, _ := pickSession(context.Background(), st, say); id != "" || len(said) != 1 {
		t.Fatalf("picking the open session gave %q and said %v", id, said)
	}
}

// /sessions on a database lists titles, latest active first, with the open one
// marked; the footer names the session by its title.
func TestStoredSessionsListTitles(t *testing.T) {
	t.Setenv("USER", "ana")
	st := &cliState{appCfg: config.Default(), store: &listingStore{MemStore: agent.NewMemStore(), rows: dbRows(time.Now())}, sessionID: "s-new"}
	out := captureStdout(t, func() { slashSessions(context.Background(), nil, nil, nil, nil, st, ui.Style{}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], "s-bo") || !strings.Contains(lines[1], "terraform cleanup") {
		t.Fatalf("/sessions printed:\n%s", out)
	}
	if !strings.Contains(out, "* s-new") || !strings.Contains(out, "Release notes") {
		t.Fatalf("the open session's title or mark is missing:\n%s", out)
	}
	if got := st.sessionLabel(); got != "Release notes" {
		t.Fatalf("the footer calls the session %q", got)
	}
	st.sessionID = "s-old"
	if got := st.sessionLabel(); got != "terraform cleanup" {
		t.Fatalf("after a switch the footer calls the session %q", got)
	}
	if got := clipLabel(strings.Repeat("x", 40), "id"); len([]rune(got)) != 30 || !strings.HasSuffix(got, "…") {
		t.Fatalf("a long label is cut to %q", got)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	was := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = was }()
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	_ = w.Close()
	return <-done
}

// /rename keeps the title rules the console and the API have, and says why
// in the same words.
func TestSlashRenameCleansTheTitle(t *testing.T) {
	for _, raw := range []string{"fix\u202etxt.exe", "a\u200bb", strings.Repeat("x", store.MaxTitleRunes+1)} {
		st := &cliState{}
		_, why := store.CleanTitle(raw)
		out := captureStdout(t, func() { slashRename(context.Background(), []string{"/rename", raw}, nil, nil, nil, st, ui.Style{}) })
		if st.pendingName != "" || why == "" || !strings.Contains(out, "not renamed: "+why) {
			t.Fatalf("%q: pending %q, said %q", raw, st.pendingName, out)
		}
	}
	st := &cliState{}
	_ = captureStdout(t, func() {
		slashRename(context.Background(), []string{"/rename", "pairing", "\U0001F468\u200d\U0001F4BB"}, nil, nil, nil, st, ui.Style{})
	})
	if st.pendingName != "pairing \U0001F468\u200d\U0001F4BB" {
		t.Fatalf("a clean title was not kept: %q", st.pendingName)
	}
}
