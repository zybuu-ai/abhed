package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

func listPageAs(t *testing.T, s *Server, query string) ([]sessionSummary, string) {
	t.Helper()
	w := callAs(t, s, "ana", "", "GET", "/v1/sessions"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions%s: %d %s", query, w.Code, w.Body.String())
	}
	var list []sessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("the list is not an array: %v: %s", err, w.Body.String())
	}
	return list, w.Header().Get(NextCursorHeader)
}

func ids(list []sessionSummary) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.ID)
	}
	return out
}

// threeSessions starts three sessions in order, each run to its end, and
// returns their ids, oldest first.
func threeSessions(t *testing.T, s *Server, st *durableMem) []string {
	t.Helper()
	var out []string
	for _, p := range []string{"fix the retry loop", "terraform cleanup", "release notes draft"} {
		id := sessionOf(t, callAs(t, s, "ana", "", "POST", "/v1/sessions", `{"prompt":"`+p+`"}`))
		turnsEnded(t, st, id, 1)
		out = append(out, id)
		time.Sleep(5 * time.Millisecond) // distinct times on a coarse clock
	}
	return out
}

// The list says when each session was last active and puts the one just
// continued first, though it was started first.
func TestSessionListIsOrderedByLastActivity(t *testing.T) {
	s, st, _, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	made := threeSessions(t, s, st)

	list, next := listPageAs(t, s, "")
	if got := ids(list); len(got) != 3 || got[0] != made[2] || got[2] != made[0] {
		t.Fatalf("before any is continued the list is %v, want newest first of %v", got, made)
	}
	if next != "" {
		t.Errorf("a list asked for whole names a next page %q", next)
	}
	for _, s := range list {
		if s.Updated.IsZero() || s.Updated.Before(s.Created) {
			t.Errorf("session %s: updated %v, created %v", s.ID, s.Updated, s.Created)
		}
	}

	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+made[0]+"/messages", `{"prompt":"and once more"}`); w.Code != http.StatusAccepted {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	events := turnsEnded(t, st, made[0], 2)
	list, _ = listPageAs(t, s, "")
	if list[0].ID != made[0] {
		t.Fatalf("the session just continued is not first: %v", ids(list))
	}
	var said time.Time
	for _, ev := range events {
		if agent.Conversational(ev) {
			said = ev.CreatedAt
		}
	}
	if !list[0].Updated.Equal(said) {
		t.Errorf("updated is %v, the conversation last went on at %v", list[0].Updated, said)
	}

	// After a restart the time still comes from the record.
	forget(s, made[0])
	if list, _ = listPageAs(t, s, ""); list[0].ID != made[0] {
		t.Fatalf("after a restart the order is %v", ids(list))
	}
}

// A search matches a title as well as the opening request, ignoring case,
// and a search that matches nothing is an empty array.
func TestSessionListSearchesTitlesAndPrompts(t *testing.T) {
	s, st, _, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	made := threeSessions(t, s, st)
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+made[1]+"/title", `{"title":"Zebra renamed"}`); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	for q, want := range map[string]string{"zebra": made[1], "ZEBRA ren": made[1], "retry": made[0], "notes draft": made[2], made[2][:6]: made[2]} {
		list, _ := listPageAs(t, s, "?q="+url.QueryEscape(q))
		if len(list) != 1 || list[0].ID != want {
			t.Errorf("q=%q found %v, want [%s]", q, ids(list), want)
		}
	}
	w := callAs(t, s, "ana", "", "GET", "/v1/sessions?q=nothing-like-this", "")
	if w.Code != http.StatusOK || w.Body.String() != "[]\n" && w.Body.String() != "[]" {
		t.Fatalf("a search with no match: %d %q", w.Code, w.Body.String())
	}
	// Another user's sessions are not searched.
	if w := callAs(t, s, "bo", "", "GET", "/v1/sessions?q=zebra", ""); w.Body.String() != "[]\n" && w.Body.String() != "[]" {
		t.Fatalf("another user's search found %s", w.Body.String())
	}
}

// Pages follow one another without a gap or a repeat, the last names no next
// page, and a malformed request is refused rather than answered whole.
func TestSessionListPages(t *testing.T) {
	s, st, _, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	made := threeSessions(t, s, st)

	first, next := listPageAs(t, s, "?limit=2")
	if len(first) != 2 || first[0].ID != made[2] || first[1].ID != made[1] || next == "" {
		t.Fatalf("page one is %v with next %q", ids(first), next)
	}
	second, after := listPageAs(t, s, "?limit=2&cursor="+url.QueryEscape(next))
	if len(second) != 1 || second[0].ID != made[0] || after != "" {
		t.Fatalf("page two is %v with next %q", ids(second), after)
	}
	// A search pages the same way.
	if list, n := listPageAs(t, s, "?q=r&limit=1"); len(list) != 1 || n == "" {
		t.Fatalf("a searched page is %v with next %q", ids(list), n)
	}
	for _, bad := range []string{"?limit=0", "?limit=201", "?limit=two", "?cursor=%25%25", "?cursor=bm90LWEtY3Vyc29y"} {
		if w := callAs(t, s, "ana", "", "GET", "/v1/sessions"+bad, ""); w.Code != http.StatusBadRequest {
			t.Errorf("GET /v1/sessions%s: %d, want 400", bad, w.Code)
		}
	}
}

// With no durable store the list is drawn from the sessions in memory, and
// still says when each was last active.
func TestSessionListInMemoryHasLastActivity(t *testing.T) {
	s := proxyServer(t)
	mem, ok := s.under().(*agent.MemStore)
	if !ok {
		t.Fatalf("the default store is %T", s.under())
	}
	id := sessionOf(t, callAs(t, s, "ana", "", "POST", "/v1/sessions", `{"prompt":"one"}`))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if at, ok := mem.LastAt(id); ok {
			list, _ := listPageAs(t, s, "")
			if len(list) == 1 && !list[0].Updated.Before(at) {
				return
			}
		}
		if time.Now().After(deadline) {
			list, _ := listPageAs(t, s, "")
			t.Fatalf("the in-memory list does not follow the record: %+v", list)
		}
	}
}

// Looking after a session is not activity: a rename and a model switch leave
// it where the conversation put it in the list.
func TestSessionListIgnoresRenamesAndModelSwitches(t *testing.T) {
	s, st, _, _ := switchServer(t, func(c *config.Config) { c.Auth.Mode = "proxy" })
	made := threeSessions(t, s, st)
	before, _ := listPageAs(t, s, "")
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+made[0]+"/title", `{"title":"Oldest, renamed"}`); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if w := callAs(t, s, "ana", "", "POST", "/v1/sessions/"+made[1]+"/model", `{"provider":"b"}`); w.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", w.Code, w.Body.String())
	}
	after, _ := listPageAs(t, s, "")
	if got, want := ids(after), ids(before); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("a rename or a model switch reordered the list: %v, was %v", got, want)
	}
	for i := range after {
		if !after[i].Updated.Equal(before[i].Updated) {
			t.Errorf("session %s: updated moved from %v to %v", after[i].ID, before[i].Updated, after[i].Updated)
		}
	}
}
