package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	{"sessions", "SELECT, INSERT, UPDATE"},
	{"approvals", "SELECT, INSERT, UPDATE"},
	{"checkpoints", "SELECT, INSERT"},
	{"users", "SELECT, INSERT, UPDATE, DELETE"},
	{"models", "SELECT"},
	{"schema_version", "SELECT"},
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

	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := pool.Exec(ctx, usersSchema); err != nil {
		return fmt.Errorf("apply users schema: %w", err)
	}
	grants := append([]struct{ table, privileges string }{}, runtimeGrants...)
	for _, ext := range cfg.Extensions {
		if _, err := pool.Exec(ctx, ext.SQL); err != nil {
			return fmt.Errorf("apply extension schema: %w", err)
		}
		for table, privs := range ext.Grants {
			grants = append(grants, struct{ table, privileges string }{table, privs})
		}
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

// validPrivileges keeps a privilege list to the keywords it should contain,
// since it is spliced into SQL and cannot be a bound parameter.
func validPrivileges(list string) bool {
	for _, p := range strings.Split(list, ",") {
		switch strings.TrimSpace(strings.ToUpper(p)) {
		case "SELECT", "INSERT", "UPDATE", "DELETE":
		default:
			return false
		}
	}
	return list != ""
}

// recordExposure reports how the connected role could alter the event record,
// or "" when it cannot. It is asked of the database rather than assumed from
// configuration: what matters is what this connection is able to do.
func recordExposure(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var owns, canUpdate, canDelete, canTruncate bool
	err := pool.QueryRow(ctx, `
		SELECT pg_has_role(current_user, c.relowner, 'MEMBER'),
		       has_table_privilege(current_user, c.oid, 'UPDATE'),
		       has_table_privilege(current_user, c.oid, 'DELETE'),
		       has_table_privilege(current_user, c.oid, 'TRUNCATE')
		FROM pg_class c WHERE c.oid = to_regclass('public.events')`).
		Scan(&owns, &canUpdate, &canDelete, &canTruncate)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNoSchema
	}
	if err != nil {
		return "", fmt.Errorf("check record privileges: %w", err)
	}
	var how []string
	if owns {
		how = append(how, "owns the events table, so it can disable the append-only triggers or drop it")
	}
	for name, can := range map[string]bool{"UPDATE": canUpdate, "DELETE": canDelete, "TRUNCATE": canTruncate} {
		if can && !owns {
			how = append(how, "holds "+name+" on events")
		}
	}
	return strings.Join(how, "; "), nil
}

var errNoSchema = errors.New("the schema has not been applied")

// requiredColumns are columns this build writes that an older schema lacks.
// The runtime role cannot add them, so a missing one is found at start rather
// than as a failed write the first time someone answers an approval.
var requiredColumns = [][2]string{{"approvals", "answer_scope"}, {"approvals", "ended_at"}}

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
