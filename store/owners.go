package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zybuu-ai/abhed/auth"
)

// Schema version 4: sessions are owned by auth.Identity.Owner.
//
// Before it, a local account's sessions were owned by its email when it had
// one and by its username otherwise, and the email was whatever the person
// typed, so an account could take another's sessions by claiming its email or
// name. A local account now owns its sessions as "local:<username>". This
// moves the rows written under the old keys, once, where the account they
// belonged to is certain, and lowercases owners that are plain emails.
const ownerSchemaVersion = 4

// UnclaimedPrefix marks a session whose old owner key more than one account
// could have held. No identity owns it; the database still has it.
const UnclaimedPrefix = "unclaimed:"

// OwnerRemap is what the owner migration did with one old owner key.
type OwnerRemap struct {
	From, To  string
	Sessions  int64
	Ambiguous bool     // To is unclaimed: Accounts all match From
	Accounts  []string // the accounts whose username or email is From
}

// reservedOwners are keys no account's sessions were ever written under: the
// owner with auth off and the CLI's subagent rows. An account named like one
// of them never takes those rows.
var reservedOwners = map[string]bool{"": true, auth.Anonymous: true, SubagentUser: true}

// ownersMigrated reports whether schema version 4 is recorded.
func ownersMigrated(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (bool, error) {
	var done bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schema_version WHERE version = $1)`, ownerSchemaVersion).Scan(&done)
	if err != nil {
		return false, fmt.Errorf("check schema version: %w", err)
	}
	return done, nil
}

// migrateOwners runs the version 4 move once, as a role that owns sessions,
// against the accounts in the users table.
func migrateOwners(ctx context.Context, pool *pgxpool.Pool) ([]OwnerRemap, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Serialises two servers starting at once; the second finds it done.
	if _, err := tx.Exec(ctx, `LOCK TABLE schema_version IN EXCLUSIVE MODE`); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	if done, err := ownersMigrated(ctx, tx); err != nil || done {
		return nil, err
	}
	accounts, err := ownerAccounts(ctx, tx)
	if err != nil {
		return nil, err
	}
	remaps, err := remapOwners(ctx, tx, accounts)
	if err != nil {
		return nil, err
	}
	folds, err := foldEmailOwners(ctx, tx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_version (version) VALUES ($1) ON CONFLICT DO NOTHING`,
		ownerSchemaVersion); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	logRemaps(remaps)
	logFolds(folds)
	return remaps, nil
}

// ownerAccounts reads every local account's username and email, or none when
// the deployment never created the accounts table.
func ownerAccounts(ctx context.Context, tx pgx.Tx) ([]*auth.User, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('users') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	if !exists {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT username, email FROM users`)
	if err != nil {
		return nil, fmt.Errorf("owner migration: read accounts: %w", err)
	}
	defer rows.Close()
	var out []*auth.User
	for rows.Next() {
		u := &auth.User{}
		if err := rows.Scan(&u.Username, &u.Email); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// remapOwners moves every session row whose owner key names exactly one
// account to that account's owner, and marks a key several accounts name as
// unclaimed. A key no account names is left alone: it belongs to another
// provider, the CLI or a schedule. Every tenant's rows are moved.
func remapOwners(ctx context.Context, tx pgx.Tx, accounts []*auth.User) ([]OwnerRemap, error) {
	holders := map[string]map[string]bool{} // folded key → usernames
	hold := func(key, username string) {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return
		}
		if holders[key] == nil {
			holders[key] = map[string]bool{}
		}
		holders[key][strings.ToLower(username)] = true
	}
	for _, u := range accounts {
		hold(u.Username, u.Username)
		hold(u.Email, u.Username)
	}
	if len(holders) == 0 {
		return nil, nil
	}

	// Row security would otherwise limit the owner to one tenant here.
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT user_id FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("owner migration: read owners: %w", err)
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []OwnerRemap
	for _, key := range keys {
		names := holders[strings.ToLower(strings.TrimSpace(key))]
		if reservedOwners[key] || len(names) == 0 {
			continue
		}
		r := OwnerRemap{From: key}
		for n := range names {
			r.Accounts = append(r.Accounts, n)
		}
		sort.Strings(r.Accounts)
		if len(r.Accounts) == 1 {
			r.To = auth.LocalOwner(r.Accounts[0])
		} else {
			r.To, r.Ambiguous = UnclaimedPrefix+key, true
		}
		tag, err := tx.Exec(ctx, `UPDATE sessions SET user_id = $1 WHERE user_id = $2`, r.To, key)
		if err != nil {
			return nil, fmt.Errorf("owner migration: move %q: %w", key, err)
		}
		r.Sessions = tag.RowsAffected()
		out = append(out, r)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions FORCE ROW LEVEL SECURITY`); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	return out, nil
}

// EmailFold is one plain-email owner the migration lowercased, with every
// case variant the rows had.
type EmailFold struct {
	To       string
	Variants []string
	Sessions int64
}

// foldEmailOwners lowercases every session owner that is a plain email, as
// auth.FoldEmailOwner does for a caller, in every tenant. Namespaced owners
// (local:, unclaimed:, oidc:) hold a ":" and are never touched.
func foldEmailOwners(ctx context.Context, tx pgx.Tx) ([]EmailFold, error) {
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT lower(user_id), array_agg(DISTINCT user_id ORDER BY user_id)
		  FROM sessions
		 WHERE strpos(user_id, '@') > 0 AND strpos(user_id, ':') = 0
		 GROUP BY lower(user_id)
		HAVING bool_or(user_id <> lower(user_id))`)
	if err != nil {
		return nil, fmt.Errorf("owner migration: read email owners: %w", err)
	}
	var out []EmailFold
	for rows.Next() {
		var f EmailFold
		if err := rows.Scan(&f.To, &f.Variants); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, f := range out {
		tag, err := tx.Exec(ctx, `UPDATE sessions SET user_id = $1
			WHERE lower(user_id) = $1 AND user_id <> $1 AND strpos(user_id, ':') = 0`, f.To)
		if err != nil {
			return nil, fmt.Errorf("owner migration: lowercase %q: %w", f.To, err)
		}
		out[i].Sessions = tag.RowsAffected()
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions FORCE ROW LEVEL SECURITY`); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	return out, nil
}

func logFolds(folds []EmailFold) {
	for _, f := range folds {
		if len(f.Variants) > 1 {
			slog.Warn("sessions under case variants of one email now share one owner",
				"owner", f.To, "variants", strings.Join(f.Variants, ","), "sessions", f.Sessions)
			continue
		}
		slog.Info("session owner email lowercased", "owner", f.To, "sessions", f.Sessions)
	}
}

func logRemaps(remaps []OwnerRemap) {
	for _, r := range remaps {
		if r.Ambiguous {
			slog.Warn("sessions left unclaimed: their old owner names more than one account",
				"old_owner", r.From, "accounts", strings.Join(r.Accounts, ","),
				"sessions", r.Sessions, "now", r.To)
			continue
		}
		slog.Info("sessions moved to their account", "old_owner", r.From, "owner", r.To, "sessions", r.Sessions)
	}
}

// errOwnersNotMigrated is Open's answer for a database the version 4 move has
// not run on: serving it would show every account an empty history.
var errOwnersNotMigrated = errors.New("the database schema is older than this server: " +
	"session owners have not been migrated (schema version 4). " +
	"Run `abhed migrate` as the owner (see docs/guide/02-configuration.md)")
