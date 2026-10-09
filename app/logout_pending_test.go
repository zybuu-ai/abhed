package app

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/profiles"
)

// A server that cannot record a sign-out is retried; when it still cannot,
// the sign-out is kept and sent again on the next command until it lands.
func TestLogoutIsNeverQuietlyLost(t *testing.T) {
	var calls, failFor atomic.Int32
	failFor.Store(3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/cli/sso/logout" || r.Header.Get("Authorization") != "Bearer tok-1" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) <= failFor.Load() {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"signed_out":true}`))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "profiles.json")
	t.Setenv(profiles.EnvFile, path)
	reg := &profiles.File{Current: "lab", Profiles: map[string]*profiles.Profile{
		"lab": {URL: srv.URL, Token: "tok-1", RevocationEndpoint: srv.URL + "/v1/cli/sso/logout",
			ExpiresAt: time.Now().Add(time.Hour)}}}
	if err := profiles.Save(path, reg); err != nil {
		t.Fatal(err)
	}
	oldA, oldB := logoutAttempts, logoutBackoff
	logoutAttempts, logoutBackoff = 2, time.Millisecond
	t.Cleanup(func() { logoutAttempts, logoutBackoff = oldA, oldB })

	ws := t.TempDir()
	var out, errOut bytes.Buffer
	if code := logoutCmd(ws, nil, &out, &errOut); code == 0 || !strings.Contains(errOut.String(), "could not record the sign-out") {
		t.Fatalf("logout against a failing server = %d %q", code, errOut.String())
	}
	if calls.Load() != 2 {
		t.Errorf("logout tried %d times, want 2", calls.Load())
	}
	reg, _ = profiles.Load(path)
	p := reg.Profiles["lab"]
	if p.Token != "" || !p.PendingLogout || len(reg.PendingLogouts) != 1 || reg.PendingLogouts[0].Token != "tok-1" {
		t.Fatalf("after a failed logout: profile %+v pending %+v", p, reg.PendingLogouts)
	}
	// The next command tries again: still failing, it stays pending and says so.
	out.Reset()
	if code := profileCmd(ws, []string{"list"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "not recorded it yet") {
		t.Fatalf("profile list = %d %q", code, out.String())
	}
	// Then the server records it: the pending sign-out is gone.
	errOut.Reset()
	if code := profileCmd(ws, []string{"list"}, &out, &errOut); code != 0 || !strings.Contains(errOut.String(), "has now recorded") {
		t.Fatalf("profile list after the server recovers = %d %q", code, errOut.String())
	}
	reg, _ = profiles.Load(path)
	if len(reg.PendingLogouts) != 0 || reg.Profiles["lab"].PendingLogout {
		t.Fatalf("the recorded sign-out is still pending: %+v", reg)
	}
	if calls.Load() != 4 {
		t.Errorf("server asked %d times, want 4", calls.Load())
	}
}
