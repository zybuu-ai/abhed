// Package testdb reads the database a test may use, and refuses one on the
// well-known Postgres port or a local socket, where a live deployment's
// database may listen.
package testdb

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// DSN is the connection string in env, "" when it is unset. A string that
// would connect to port 5432, or to a unix socket, by any of its hosts or
// fallbacks fails the test: a throwaway test database is given a TCP port
// of its own, and a mistake must never reach a live one.
func DSN(t testing.TB, env string) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(env))
	if dsn == "" {
		return ""
	}
	if why := Check(dsn); why != "" {
		t.Fatalf("%s: %s", env, why)
	}
	return dsn
}

// Check says why dsn may not be used, or "" when it may. It reads dsn as
// the driver will, query overrides, PG* defaults and fallbacks included.
func Check(dsn string) string {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "it does not parse as a connection string: " + err.Error()
	}
	hosts := []struct {
		host string
		port uint16
	}{{cfg.Host, cfg.Port}}
	for _, f := range cfg.Fallbacks {
		hosts = append(hosts, struct {
			host string
			port uint16
		}{f.Host, f.Port})
	}
	for _, h := range hosts {
		switch {
		case h.host == "" || strings.HasPrefix(h.host, "/"):
			return "it reaches a unix socket (" + h.host + "), where a live database may listen; give the test database a TCP port of its own"
		case h.port == 5432:
			return "it reaches port 5432 (" + h.host + "), where a live database may listen; give the test database another port"
		}
	}
	return ""
}
