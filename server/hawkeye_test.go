package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/hawkeye"
)

// A report carries every tool result in a session, so it answers to the same
// boundary replay does: another tenant must not learn the session exists.
func TestHawkeyeIsScopedLikeReplay(t *testing.T) {
	s := proxyServer(t)
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(`{"prompt":"work"}`))
	req.Header.Set("X-Abhed-Tenant", "acme")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body)
	}
	var created createResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	time.Sleep(200 * time.Millisecond) // let the loop finish

	get := func(tenant, query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/v1/sessions/"+created.SessionID+"/hawkeye"+query, nil)
		req.Header.Set("X-Abhed-Tenant", tenant)
		h.ServeHTTP(rec, req)
		return rec
	}

	if got := get("other", ""); got.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant report must 404, got %d", got.Code)
	}

	own := get("acme", "")
	if own.Code != http.StatusOK {
		t.Fatalf("owner's report: %d %s", own.Code, own.Body)
	}
	var rep hawkeye.Report
	if err := json.Unmarshal(own.Body.Bytes(), &rep); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	if rep.SessionID != created.SessionID || rep.Totals.Events == 0 {
		t.Fatalf("report is empty: %+v", rep.Totals)
	}
	if len(rep.Turns) == 0 {
		t.Fatal("no per-turn accounting: the loop is not recording model.call")
	}
	if len(rep.Integrity.Gaps) != 0 {
		t.Fatalf("a live record has gaps: %v", rep.Integrity.Gaps)
	}

	page := get("acme", "?format=html")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "HawkEYE") {
		t.Fatalf("html report: %d", page.Code)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("the report page must forbid scripts and loads, got CSP %q", csp)
	}
}

func TestHawkeyeUnknownSessionIs404(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions/nope/hawkeye", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}
