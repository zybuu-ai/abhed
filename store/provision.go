package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zybuu-ai/abhed/auth"
)

// The record is only as protected as the role that writes it. Triggers refuse
// an UPDATE, a DELETE or a TRUNCATE, but the role that owns a table may
// disable its triggers or drop it, and nothing inside the database can stop
// an owner. So the schema is applied by one role and the server runs as
// another, which holds exactly the privileges below and owns nothing.

// runtimeGrants is everything the running server needs, and no more. events
// gets INSERT and SELECT: there is no statement in this package that changes
// or removes an event, so the role is not given the means to.
var runtimeGrants = []struct{ table, privileges string }{
	{"events", "SELECT, INSERT"},
	// Only the columns a session's life changes: never its id, tenant,
	// parent or start, and its owner only as the sessions_guard trigger allows.
	{"sessions", "SELECT, INSERT, UPDATE (user_id, model, mode, prompt, title, ended_at, terminal_reason, turns, " +
		"tokens_in, tokens_out, tokens_cached, compactions, gpu_seconds, cost_usd, context_tokens, context_window, " +
		"node_id, node_seen_at, deleted_at, deleted_by)"},
	{"owner_changes", "SELECT"},
	// A key is deleted to erase its session; it is never rewritten.
	{"session_keys", "SELECT, INSERT, DELETE"},
	{"approvals", "SELECT, INSERT, UPDATE"},
	{"checkpoints", "SELECT, INSERT"},
	{"users", "SELECT, INSERT, UPDATE, DELETE"},
	{"account_changes", "SELECT"},
	{"models", "SELECT"},
	{"schema_version", "SELECT"},
	// The control plane: sandboxes are rewritten and removed by design; its
	// audit table is a record, so insert and read only.
	{"sandboxes", "SELECT, INSERT, UPDATE, DELETE"},
	{"sandbox_idempotency", "SELECT, INSERT, UPDATE, DELETE"},
	{"sandbox_audit", "SELECT, INSERT"},
	{"sandbox_templates", "SELECT, INSERT, DELETE"},
}

// Extension lets an edition add its own tables in the same pass, so they are
// owned and granted on the same terms as the core schema.
type Extension struct {
	SQL    string
	Grants map[string]string // table → privileges for the runtime role
}

// ProvisionConfig describes one schema application.
type ProvisionConfig struct {
	// OwnerDSN connects as the role that will own the tables. It is used here
	// and nowhere else; the running server never sees it.
	OwnerDSN string
	// RuntimeRole is the role the server connects as. It must already exist
	// and must differ from the owner.
	RuntimeRole string
	Extensions  []Extension
	// Owners is what the owner migration may do with rows keyed by a local
	// account's name or email; empty means OwnersUnclaim.
	Owners OwnerPolicy
	// OwnerAccounts, AllowNoAccounts and AccountsFound are passed to the
	// owner migration; see OwnerMigration.
	OwnerAccounts   []*auth.User
	AllowNoAccounts bool
	AccountsFound   func(table, extra, distinct int)
}

// Provision applies the schema as the owner and grants the runtime role what
// it needs. Idempotent: privileges are revoked and granted afresh each time,
// so a privilege removed from the list is removed from the database too.
func Provision(ctx context.Context, cfg ProvisionConfig) error {
	if cfg.RuntimeRole == "" {
		return errors.New("provision: no runtime role named")
	}
	pool, err := pgxpool.New(ctx, cfg.OwnerDSN)
	if err != nil {
		return fmt.Errorf("connect as owner: %w", err)
	}
	defer pool.Close()

	var owner string
	if err := pool.QueryRow(ctx, `SELECT current_user`).Scan(&owner); err != nil {
		return fmt.Errorf("connect as owner %s: %w", redactDSN(cfg.OwnerDSN), err)
	}
	if owner == cfg.RuntimeRole {
		return fmt.Errorf("provision: the owner and the runtime role are both %q. "+
			"The point is that they differ: the role that runs the server must not own the record", owner)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`,
		cfg.RuntimeRole).Scan(&exists); err != nil {
		return fmt.Errorf("look up runtime role: %w", err)
	}
	if !exists {
		return fmt.Errorf("provision: runtime role %q does not exist. Create it first: "+
			"CREATE ROLE %s LOGIN PASSWORD '…'", cfg.RuntimeRole, pgx.Identifier{cfg.RuntimeRole}.Sanitize())
	}

	if err := applySchema(ctx, pool); err != nil {
		return err
	}
	if err := migrateUsersSchema(ctx, pool); err != nil {
		return err
	}
	grants := append([]struct{ table, privileges string }{}, runtimeGrants...)
	for _, ext := range cfg.Extensions {
		if _, err := pool.Exec(ctx, ext.SQL); err != nil {
			return fmt.Errorf("apply extension schema: %w", err)
		}
		for table, privs := range ext.Grants {
			if coreTable(table) {
				return fmt.Errorf("provision: an extension may not grant on the core table %q", table)
			}
			grants = append(grants, struct{ table, privileges string }{table, privs})
		}
	}

	if _, err := migrateOwners(ctx, pool, OwnerMigration{Policy: cfg.Owners,
		Accounts: cfg.OwnerAccounts, AllowNoAccounts: cfg.AllowNoAccounts, Found: cfg.AccountsFound}); err != nil {
		return err
	}

	role := pgx.Identifier{cfg.RuntimeRole}.Sanitize()
	// Sequences too: an edition's serial column is useless to a role that
	// may insert but cannot draw the next id.
	stmts := []string{
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO " + role,
	}
	for _, g := range grants {
		table := pgx.Identifier{g.table}.Sanitize()
		if !validPrivileges(g.privileges) {
			return fmt.Errorf("provision: %q is not a privilege list", g.privileges)
		}
		stmts = append(stmts,
			"REVOKE ALL ON "+table+" FROM "+role,
			"GRANT "+g.privileges+" ON "+table+" TO "+role)
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// coreTable reports whether table is granted by runtimeGrants, which an
// extension's grant would otherwise replace.
func coreTable(table string) bool {
	for _, g := range runtimeGrants {
		if strings.EqualFold(g.table, table) {
			return true
		}
	}
	return false
}

// validPrivileges keeps a privilege list to the keywords it should contain,
// since it is spliced into SQL and cannot be a bound parameter. SELECT, INSERT
// and UPDATE may name columns, so a role can be held to the columns it writes.
func validPrivileges(list string) bool {
	return privilegeList.MatchString(list)
}

var privilegeList = func() *regexp.Regexp {
	item := `(?:(?i:DELETE)|(?i:SELECT|INSERT|UPDATE)(?:\s*\(\s*[a-z_][a-z0-9_]*(?:\s*,\s*[a-z_][a-z0-9_]*)*\s*\))?)`
	return regexp.MustCompile(`^\s*` + item + `(?:\s*,\s*` + item + `)*\s*$`)
}()

// recordTables are the append-only records: the session events and the
// control plane's sandbox audit, which an older schema does not have yet.
var recordTables = []string{"events", "sandbox_audit"}

// recordExposure reports how the connected role could alter an append-only
// record, or "" when it cannot. It is asked of the database rather than
// assumed from configuration: what matters is what this connection is able to do.
func recordExposure(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var how []string
	for _, table := range recordTables {
		var owns, canUpdate, canDelete, canTruncate bool
		err := pool.QueryRow(ctx, `
			SELECT pg_has_role(current_user, c.relowner, 'MEMBER'),
			       has_any_column_privilege(current_user, c.oid, 'UPDATE'),
			       has_table_privilege(current_user, c.oid, 'DELETE'),
			       has_table_privilege(current_user, c.oid, 'TRUNCATE')
			FROM pg_class c WHERE c.oid = to_regclass('public.' || $1)`, table).
			Scan(&owns, &canUpdate, &canDelete, &canTruncate)
		if errors.Is(err, pgx.ErrNoRows) {
			if table == "events" {
				return "", errNoSchema
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("check record privileges: %w", err)
		}
		if owns {
			how = append(how, "owns the "+table+" table, so it can disable the append-only triggers or drop it")
			continue
		}
		for _, p := range []struct {
			name string
			can  bool
		}{{"UPDATE", canUpdate}, {"DELETE", canDelete}, {"TRUNCATE", canTruncate}} {
			if p.can {
				how = append(how, "holds "+p.name+" on "+table)
			}
		}
	}
	return strings.Join(how, "; "), nil
}

var errNoSchema = errors.New("the schema has not been applied")

// requiredColumns are columns this build writes that an older schema lacks.
// The runtime role cannot add them, so a missing one is found at start rather
// than as a failed write the first time someone answers an approval.
var requiredColumns = [][2]string{{"approvals", "answer_scope"}, {"approvals", "ended_at"}, {"approvals", "request_id"}, {"users", "revocations"}, {"sessions", "title"},
	{"events", "inserted_at"}, {"events", "hash"}, {"owner_changes", "by_role"}, {"session_keys", "key"}}

// missingColumns names the required columns the connected database lacks.
func missingColumns(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	var missing []string
	for _, c := range requiredColumns {
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, c[0], c[1]).Scan(&n); err != nil {
			return nil, fmt.Errorf("check schema: %w", err)
		}
		if n == 0 {
			missing = append(missing, c[0]+"."+c[1])
		}
	}
	return missing, nil
}
