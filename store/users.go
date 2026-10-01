package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zybuu-ai/abhed/auth"
)

// Local accounts, stored alongside the event log.
//
// Kept in the same database as sessions so a deployment has one thing to back
// up and one thing to restore. Accounts are NOT tenant-scoped by row-level
// security: a user's tenant is a property of the account, so scoping the table
// by tenant would make it impossible to look someone up before knowing which
// tenant they belong to.

const usersSchema = `
CREATE TABLE IF NOT EXISTS users (
  username    TEXT PRIMARY KEY,
  email       TEXT        NOT NULL DEFAULT '',
  name        TEXT        NOT NULL DEFAULT '',
  tenant      TEXT        NOT NULL DEFAULT 'default',
  groups      TEXT        NOT NULL DEFAULT '',
  hash        TEXT        NOT NULL,
  must_change BOOLEAN     NOT NULL DEFAULT false,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS users_tenant_idx ON users (tenant);
CREATE INDEX IF NOT EXISTS users_email_idx  ON users (email) WHERE email <> '';

-- A sign-out everywhere, counted so every server ends older sessions.
ALTER TABLE users ADD COLUMN IF NOT EXISTS revocations BIGINT NOT NULL DEFAULT 0;
`

// MigrateUsers creates the accounts table. Separate from the main schema so a
// deployment using OIDC never creates a table it will not use.
//
// Guarded by a sync.Once because the callers below invoke it defensively on
// every operation, and Authenticate is on the sign-in path: without the guard
// each password check ran a CREATE TABLE statement first, taking DDL locks
// against a table that already existed.
func (p *Postgres) MigrateUsers(ctx context.Context) error {
	p.usersOnce.Do(func() {
		// A runtime role owns nothing and cannot create a table; Provision
		// made this one alongside the rest.
		if p.protected {
			return
		}
		p.usersErr = migrateUsersSchema(ctx, p.pool)
	})
	return p.usersErr
}

// Schema version 5: no two accounts share a username or an email, ignoring
// case, enforced by the database so two processes cannot both create one.
const accountKeysSchemaVersion = 5

const accountKeysSchema = `
CREATE UNIQUE INDEX IF NOT EXISTS users_username_key ON users (lower(username));
CREATE UNIQUE INDEX IF NOT EXISTS users_email_key ON users (lower(btrim(email))) WHERE btrim(email) <> '';
`

// migrateUsersSchema applies the accounts table and the version 5 indexes in
// one transaction, one process at a time, refusing with the accounts to fix
// when existing rows already share a key. Idempotent.
func migrateUsersSchema(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("apply users schema: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Two processes applying the DDL at once deadlock on the table's locks.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('abhed.users.schema'))`); err != nil {
		return fmt.Errorf("apply users schema: %w", err)
	}
	if _, err := tx.Exec(ctx, usersSchema); err != nil {
		return fmt.Errorf("apply users schema: %w", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("account keys migration: %w", err)
	}
	if err := applyAccountKeys(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// applyAccountKeys refuses existing accounts that share a key, naming them,
// else adds the version 5 indexes and records the version, in tx.
func applyAccountKeys(ctx context.Context, tx pgx.Tx) error {
	clashes, err := accountKeyClashes(ctx, tx)
	if err != nil {
		return err
	}
	if len(clashes) > 0 {
		return fmt.Errorf("cannot make account usernames and emails unique (schema version 5): %s. "+
			"Give all but one of each another email (or remove the extra accounts) and run `abhed migrate` again",
			strings.Join(clashes, "; "))
	}
	if _, err := tx.Exec(ctx, accountKeysSchema); err != nil {
		return fmt.Errorf("account keys migration: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_version (version) VALUES ($1) ON CONFLICT DO NOTHING`,
		accountKeysSchemaVersion); err != nil {
		return fmt.Errorf("account keys migration: %w", err)
	}
	return nil
}

// accountKeyClashes names the accounts that already share a username or an
// email, ignoring case, which the version 5 indexes would refuse.
func accountKeyClashes(ctx context.Context, tx pgx.Tx) ([]string, error) {
	var clashes []string
	for _, q := range []struct{ key, sql string }{
		{"username", `SELECT lower(username), string_agg(username, ', ' ORDER BY username)
			FROM users GROUP BY 1 HAVING count(*) > 1`},
		{"email", `SELECT lower(btrim(email)), string_agg(username, ', ' ORDER BY username)
			FROM users WHERE btrim(email) <> '' GROUP BY 1 HAVING count(*) > 1`},
	} {
		rows, err := tx.Query(ctx, q.sql)
		if err != nil {
			return nil, fmt.Errorf("account keys migration: %w", err)
		}
		for rows.Next() {
			var key, names string
			if err := rows.Scan(&key, &names); err != nil {
				rows.Close()
				return nil, fmt.Errorf("account keys migration: %w", err)
			}
			clashes = append(clashes, fmt.Sprintf("accounts %s share the %s %q", names, q.key, key))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("account keys migration: %w", err)
		}
	}
	return clashes, nil
}

// errAccountKeysNotMigrated is Open's answer for a database without version 5.
var errAccountKeysNotMigrated = errors.New("the database schema is older than this server: " +
	"account usernames and emails are not unique yet (schema version 5). " +
	"Run `abhed migrate` as the owner (see docs/guide/02-configuration.md)")

// accountKeyErr turns a duplicate-key violation into the error CreateUser
// would have given had it seen the other account first.
func accountKeyErr(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" {
		return err
	}
	if pe.ConstraintName == "users_email_key" {
		return auth.ErrEmailTaken
	}
	return auth.ErrUserExists
}

// Create adds an account, failing with auth.ErrUserExists or
// auth.ErrEmailTaken when another holds its username or email.
func (p *Postgres) Create(ctx context.Context, u *auth.User) error {
	if err := p.MigrateUsers(ctx); err != nil {
		return err
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO users (username, email, name, tenant, groups, hash, must_change, created_at, revocations)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		strings.ToLower(u.Username), u.Email, u.Name, u.Tenant,
		strings.Join(u.Groups, ","), u.Hash, u.MustChange, u.CreatedAt, u.Revocations)
	return accountKeyErr(err)
}

func (p *Postgres) Get(ctx context.Context, username string) (*auth.User, error) {
	if err := p.MigrateUsers(ctx); err != nil {
		return nil, err
	}
	var u auth.User
	var groups string
	err := p.pool.QueryRow(ctx, `
		SELECT username, email, name, tenant, groups, hash, must_change, created_at, revocations
		FROM users WHERE username = $1`, strings.ToLower(username)).
		Scan(&u.Username, &u.Email, &u.Name, &u.Tenant, &groups,
			&u.Hash, &u.MustChange, &u.CreatedAt, &u.Revocations)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, auth.ErrNoSuchUser
	}
	if err != nil {
		return nil, err
	}
	if groups != "" {
		u.Groups = strings.Split(groups, ",")
	}
	return &u, nil
}

// Put stores an update to an account; CreateUser adds one through Create.
func (p *Postgres) Put(ctx context.Context, u *auth.User) error {
	if err := p.MigrateUsers(ctx); err != nil {
		return err
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	// The count only rises, so a write from an older read cannot undo a revocation.
	_, err := p.pool.Exec(ctx, `
		INSERT INTO users (username, email, name, tenant, groups, hash, must_change, created_at, revocations)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (username) DO UPDATE SET
		  email = EXCLUDED.email, name = EXCLUDED.name, tenant = EXCLUDED.tenant,
		  groups = EXCLUDED.groups, hash = EXCLUDED.hash,
		  must_change = EXCLUDED.must_change,
		  revocations = GREATEST(users.revocations, EXCLUDED.revocations)`,
		strings.ToLower(u.Username), u.Email, u.Name, u.Tenant,
		strings.Join(u.Groups, ","), u.Hash, u.MustChange, u.CreatedAt, u.Revocations)
	return accountKeyErr(err)
}

// AddRevocation raises the account's revocation count and returns it.
func (p *Postgres) AddRevocation(ctx context.Context, username string) (int64, error) {
	if err := p.MigrateUsers(ctx); err != nil {
		return 0, err
	}
	var n int64
	err := p.pool.QueryRow(ctx, `
		UPDATE users SET revocations = revocations + 1 WHERE username = $1
		RETURNING revocations`, strings.ToLower(username)).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, auth.ErrNoSuchUser
	}
	return n, err
}

func (p *Postgres) List(ctx context.Context) ([]*auth.User, error) {
	if err := p.MigrateUsers(ctx); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
		SELECT username, email, name, tenant, groups, must_change, created_at
		FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*auth.User
	for rows.Next() {
		var u auth.User
		var groups string
		if err := rows.Scan(&u.Username, &u.Email, &u.Name, &u.Tenant,
			&groups, &u.MustChange, &u.CreatedAt); err != nil {
			return nil, err
		}
		if groups != "" {
			u.Groups = strings.Split(groups, ",")
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

func (p *Postgres) Delete(ctx context.Context, username string) error {
	_, err := p.RemoveUser(ctx, username)
	return err
}

// RemoveUser deletes an account and, in the same transaction, moves the
// sessions it owned in its tenant to auth.UnclaimedOwner, so an account made
// later under the same name does not inherit them. It returns how many moved.
func (p *Postgres) RemoveUser(ctx context.Context, username string) (int64, error) {
	if err := p.MigrateUsers(ctx); err != nil {
		return 0, err
	}
	name := strings.ToLower(username)
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenant string
	err = tx.QueryRow(ctx, `SELECT tenant FROM users WHERE username = $1 FOR UPDATE`, name).Scan(&tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, auth.ErrNoSuchUser
	}
	if err != nil {
		return 0, err
	}
	// Row-level security shows one tenant: the account's, for this transaction only.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		return 0, err
	}
	owner := auth.LocalOwner(name)
	tag, err := tx.Exec(ctx, `UPDATE sessions SET user_id = $1 WHERE tenant_id = $2 AND user_id = $3`,
		auth.UnclaimedOwner(owner), tenant, owner)
	if err != nil {
		return 0, fmt.Errorf("unclaim sessions of %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE username = $1`, name); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
