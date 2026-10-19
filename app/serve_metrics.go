package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
	"github.com/zybuu-ai/abhed/store/local"
)

// MetricsPath is where the metrics are served.
const MetricsPath = "/metrics"

// serveMetrics is what server.metrics asks of serve: the metric set, and
// whether it is mounted on the API or served on a listener of its own.
type serveMetrics struct {
	m      *server.Metrics
	listen string
	token  string
}

// newServeMetrics reads server.metrics; nil when it is off.
func newServeMetrics(cfg config.Config) (*serveMetrics, error) {
	mc := cfg.Server.Metrics
	if !mc.Enabled {
		return nil, nil
	}
	sm := &serveMetrics{m: server.NewMetrics(), listen: mc.Listen, token: mc.MetricsToken()}
	if mc.TokenEnv != "" && sm.token == "" {
		return nil, fmt.Errorf("server.metrics.token_env names %s, which is unset or empty", mc.TokenEnv)
	}
	return sm, nil
}

// mount serves the metrics on the API, to administrators, when no listener
// of their own was asked for.
func (sm *serveMetrics) mount(s *server.Server, mux *http.ServeMux) {
	if sm.listen != "" {
		return
	}
	mux.Handle("GET "+MetricsPath, s.Admin(sm.m.Handler().ServeHTTP))
}

// handler is the separate listener's: /metrics only, behind the token when
// one is set.
func (sm *serveMetrics) handler() http.Handler {
	mux := http.NewServeMux()
	reg := sm.m.Handler()
	mux.HandleFunc("GET "+MetricsPath, func(w http.ResponseWriter, r *http.Request) {
		if sm.token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(sm.token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="abhed metrics"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		reg.ServeHTTP(w, r)
	})
	return mux
}

// run serves the separate listener until ctx ends; it returns once listening.
func (sm *serveMetrics) run(ctx context.Context) (string, error) {
	ln, err := net.Listen("tcp", sm.listen)
	if err != nil {
		return "", fmt.Errorf("server.metrics.listen: %w", err)
	}
	hs := &http.Server{Handler: sm.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
	go func() { _ = hs.Serve(ln) }()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	return ln.Addr().String(), nil
}

// label is the banner's line for the metrics.
func (sm *serveMetrics) label(addr string, signIn bool) string {
	if sm.listen == "" {
		if !signIn {
			return MetricsPath + " on the API, for administrators; with no sign-in there are none, so set server.metrics.listen"
		}
		return MetricsPath + " on the API, for administrators"
	}
	if sm.token != "" {
		return "http://" + addr + MetricsPath + " (bearer token)"
	}
	return "http://" + addr + MetricsPath
}

// auditHook counts the control plane's audit events, the sandbox operations
// among them, and how long their writes take.
func (sm *serveMetrics) auditHook() ControlPlaneHook {
	return func(_ context.Context, _ config.Config, c *controlplane.Config) error {
		if c.Audit != nil {
			c.Audit = sm.m.AuditLog(c.Audit)
		}
		return nil
	}
}

// apiAudit is the chained record serve writes each mutating API request to:
// the tenant's control-plane audit table beside the sessions on Postgres,
// which a trigger chains, or else a chained file in the local record.
func apiAudit(ctx context.Context, cfg config.Config, events server.EventStore) (controlplane.AuditLog, string, func(), error) {
	var pg *store.Postgres
	switch st := events.(type) {
	case *store.Postgres:
		pg = st
	case *store.Held:
		pg = st.Postgres
	}
	if pg != nil {
		q, err := pg.Sandboxes(ctx)
		if err != nil {
			return nil, "", nil, fmt.Errorf("the API audit record: %w", err)
		}
		return q, "postgres sandbox_audit (tenant " + pg.Tenant() + ", chained)", func() {}, nil
	}
	if cfg.Storage.Driver == "postgres" {
		return nil, "", nil, errors.New("storage.driver is postgres but the event store is not")
	}
	f, err := local.OpenAudit(cfg.Record.Dir, orDefault(cfg.Storage.Tenant, "default"))
	if err != nil {
		return nil, "", nil, fmt.Errorf("the API audit record: %w", err)
	}
	return f, f.Path() + " (chained)", func() { _ = f.Close() }, nil
}
