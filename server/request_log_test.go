package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A request refused before it is identified, a 401 for no sign-in or a 429
// from the sign-in throttle, has its line in the request log too, once.
func TestRequestLogKeepsRefusals(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	cfg.Auth.Mode = "local"
	local := auth.NewLocalAuth(auth.NewMemoryUserStore(), time.Hour, false)
	h := New(Options{Workspace: t.TempDir(), Config: cfg, Adapter: stubAdapter{},
		Registry: tools.NewRegistry(tools.Read{}), Logger: slog.New(slog.NewTextHandler(&buf, nil)),
		Auth: &auth.Middleware{Providers: []auth.Provider{local},
			PublicPaths: append(PublicPaths(), local.PublicPaths()...)}}).Handler()
	for i := 0; i < 12; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/signin",
			strings.NewReader(`{"username":"nobody","password":"wrong-password-1"}`)))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned request = %d", rec.Code)
	}
	logged := buf.String()
	for _, want := range []string{
		"path=/v1/signin status=401", "path=/v1/signin status=429", "path=/v1/sessions status=401",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the request log has no line with %q:\n%s", want, logged)
		}
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/health", nil))
	if n := strings.Count(buf.String(), "path=/v1/health"); n != 1 {
		t.Errorf("a served request was logged %d times", n)
	}
}
