package store

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Every table schema.sql creates must be granted to the runtime role, locked
// before a migrate alters it, and, when it is per tenant, isolated by forced
// row security; otherwise a table added by one branch starts unusable or open.
func TestSchemaTablesAreGrantedLockedAndIsolated(t *testing.T) {
	created := regexp.MustCompile(`(?ms)^CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\);`).FindAllStringSubmatch(schemaSQL, -1)
	if len(created) < 10 {
		t.Fatalf("found only %d tables in schema.sql; the pattern no longer matches it", len(created))
	}
	for _, m := range created {
		table, body := m[1], m[2]
		if !slices.ContainsFunc(runtimeGrants, func(g struct{ table, privileges string }) bool { return g.table == table }) {
			t.Errorf("%s has no runtime grant", table)
		}
		if !strings.Contains(lockForSchema, "'"+table+"'") {
			t.Errorf("%s is not locked before the schema is applied", table)
		}
		if strings.Contains(body, "tenant_id") &&
			!regexp.MustCompile(`ALTER TABLE `+table+`\s+FORCE ROW LEVEL SECURITY`).MatchString(schemaSQL) {
			t.Errorf("%s holds tenants' rows but its row security is not forced", table)
		}
	}
}

// The control plane's tables are all in schema version 7, recorded once, and
// each is checked for before the store serves a request.
func TestControlPlaneTablesAreOneSchemaVersion(t *testing.T) {
	if n := strings.Count(schemaSQL, "INSERT INTO schema_version (version) VALUES (7)"); n != 1 {
		t.Fatalf("schema version 7 recorded %d times", n)
	}
	at := strings.Index(schemaSQL, "-- Schema version 7")
	if at < 0 {
		t.Fatal("schema.sql has no schema version 7")
	}
	v7 := schemaSQL[at:]
	if end := strings.Index(v7, "-- Schema version 8"); end > 0 {
		v7 = v7[:end]
	}
	for _, m := range regexp.MustCompile(`(?m)^CREATE TABLE IF NOT EXISTS (\w+)`).FindAllStringSubmatch(v7, -1) {
		if !slices.ContainsFunc(sandboxColumns, func(c [2]string) bool { return c[0] == m[1] }) {
			t.Errorf("%s is not among the columns the control plane checks for", m[1])
		}
	}
}
