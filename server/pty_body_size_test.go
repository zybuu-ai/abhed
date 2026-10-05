package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The workbench's command and terminal endpoints refuse a body over their cap
// as too large, as the rest of the server does, and a bad small one as 400.
func TestWorkbenchOversizedBodyIs413(t *testing.T) {
	wb := manualBench(t, nil)
	raw := func(endpoint, body string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/sessions/"+wb.session+"/"+endpoint, strings.NewReader(body))
		req.Header.Set("X-Abhed-Tenant", "acme")
		wb.h.ServeHTTP(rec, req)
		return rec.Code
	}
	big := `{"command":"` + strings.Repeat("a", maxJSONBody+10) + `"}`
	start := wb.startPTY("cat")
	t.Cleanup(func() { wb.send("acme", "DELETE", "pty/"+start.ID, nil) })
	for endpoint, body := range map[string]string{
		"exec":                        big,
		"pty":                         big,
		"pty/" + start.ID + "/resize": big,
		"pty/" + start.ID + "/input":  strings.Repeat("a", 64<<10+10),
		"title":                       big,
		"fork":                        big,
	} {
		if code := raw(endpoint, body); code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s oversized: %d", endpoint, code)
		}
	}
	for _, endpoint := range []string{"exec", "pty", "pty/" + start.ID + "/resize", "title", "fork"} {
		if code := raw(endpoint, `{"command":`); code != http.StatusBadRequest {
			t.Errorf("%s malformed: %d", endpoint, code)
		}
	}
}

// The rename and fork handlers cap their own body, as the middleware does, so
// a route mounted without it still answers 413 and never reads past the cap.
func TestRenameAndForkCapTheirOwnBody(t *testing.T) {
	wb := manualBench(t, nil)
	for name, h := range map[string]http.HandlerFunc{"title": wb.s.renameSession, "fork": wb.s.forkSession} {
		rec := httptest.NewRecorder()
		body := `{"title":"` + strings.Repeat("a", maxJSONBody+10) + `","before_seq":1}`
		req := httptest.NewRequest("POST", "/v1/sessions/"+wb.session+"/"+name, strings.NewReader(body))
		req.SetPathValue("id", wb.session)
		h(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s oversized, called directly: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}
