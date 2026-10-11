package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zybuu-ai/abhed/internal/egress"
)

// Under the allowlist, a cluster no rule allows is refused before any
// request reaches it, and the decision is recorded as kind k8s.
func TestClusterRequestsAreJudgedByTheAllowlist(t *testing.T) {
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	c, err := OpenLogin(LoginCluster{Name: "c", Server: srv.URL, InsecureSkipTLSVerify: true}, "", "tok-123456789", "default")
	if err != nil {
		t.Fatal(err)
	}
	pol, err := egress.Compile(egress.Config{Rules: []egress.Rule{{Host: "other.test", Decision: "allow"}}})
	if err != nil {
		t.Fatal(err)
	}
	g := egress.NewGuard(egress.GuardOptions{Policy: pol})
	defer g.Close()
	var kinds []any
	rec := func(_ string, m map[string]any) error { kinds = append(kinds, m["kind"]); return nil }
	ctx := egress.WithGuard(egress.WithCaller(context.Background(), egress.Caller{Session: "s", CallID: "c", Record: rec}), g)
	_, err = c.Do(ctx, http.MethodGet, "/api", nil)
	var de *egress.DeniedError
	if !errors.As(err, &de) || hits != 0 {
		t.Fatalf("the cluster was reached or not refused: %v (%d hits)", err, hits)
	}
	g.EndSession("s")
	if len(kinds) == 0 || kinds[0] != egress.KindK8s {
		t.Fatalf("recorded kinds: %v", kinds)
	}
}
