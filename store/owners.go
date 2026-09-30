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

// OwnerPolicy says what the migration may do with a row whose old owner key
// is a local account's username or email.
type OwnerPolicy string

const (
	// OwnersUnclaim marks every such row unclaimed. It is the default, and
	// the only safe answer where anything other than local accounts signed
	// people in: a proxy user or single sign-on identity could have written
	// under the same name or address, which a local account may merely have
	// typed.
	OwnersUnclaim OwnerPolicy = "unclaim"
	// OwnersLocalOnly moves a row to the one account in its tenant the key
	// names. Only for a deployment where local accounts were the only way in.
	OwnersLocalOnly OwnerPolicy = "local-only"
)

// ParseOwnerPolicy reads the value of `abhed migrate --owners`.
func ParseOwnerPolicy(s string) (OwnerPolicy, error) {
	switch OwnerPolicy(s) {
	case OwnersUnclaim, OwnersLocalOnly:
		return OwnerPolicy(s), nil
	}
	return "", fmt.Errorf("--owners must be %q or %q, not %q", OwnersLocalOnly, OwnersUnclaim, s)
}

// OwnerRemap is what the owner migration did with one old owner key in one
// tenant.
type OwnerRemap struct {
	Tenant    string
	From, To  string
	Sessions  int64
	Ambiguous bool     // To is unclaimed rather than an account
	Accounts  []string // the tenant's accounts whose username or email is From
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

// checkOwnersMigrated is Open's check, a variable so a test can prove the
// refusal without unrecording version 4 in a shared database.
var checkOwnersMigrated = func(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	return ownersMigrated(ctx, pool)
}

// migrateOwners runs the version 4 move once, as a role that owns sessions,
// against the accounts in the users table.
func migrateOwners(ctx context.Context, pool *pgxpool.Pool, policy OwnerPolicy) ([]OwnerRemap, error) {
	if policy != OwnersLocalOnly {
		policy = OwnersUnclaim
	}
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
	slog.Info("migrating session owners (schema version 4)", "owners", string(policy))
	remaps, err := remapOwners(ctx, tx, accounts, policy)
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
	rows, err := tx.Query(ctx, `SELECT username, email, tenant FROM users`)
	if err != nil {
		return nil, fmt.Errorf("owner migration: read accounts: %w", err)
	}
	defer rows.Close()
	var out []*auth.User
	for rows.Next() {
		u := &auth.User{}
		if err := rows.Scan(&u.Username, &u.Email, &u.Tenant); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// remapOwners handles every session row whose owner key is, folded, the
// username or email of an account in the row's own tenant. Under
// OwnersLocalOnly a key one such account holds moves to it and a key several
// hold is unclaimed; under OwnersUnclaim every such key is unclaimed. A key
// no account in the tenant names is left alone: it belongs to another
// provider, the CLI or a schedule.
func remapOwners(ctx context.Context, tx pgx.Tx, accounts []*auth.User, policy OwnerPolicy) ([]OwnerRemap, error) {
	type slot struct{ tenant, key string }
	holders := map[slot]map[string]bool{} // (tenant, folded key) → usernames
	hold := func(tenant, key, username string) {
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return
		}
		k := slot{tenant, key}
		if holders[k] == nil {
			holders[k] = map[string]bool{}
		}
		holders[k][strings.ToLower(username)] = true
	}
	for _, u := range accounts {
		tenant := u.Tenant
		if tenant == "" {
			tenant = "default"
		}
		hold(tenant, u.Username, u.Username)
		hold(tenant, u.Email, u.Username)
	}
	if len(holders) == 0 {
		return nil, nil
	}

	// Row security would otherwise limit the owner to one tenant here.
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT tenant_id, user_id FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("owner migration: read owners: %w", err)
	}
	var keys []slot
	for rows.Next() {
		var k slot
		if err := rows.Scan(&k.tenant, &k.key); err != nil {
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
	for _, k := range keys {
		names := holders[slot{k.tenant, strings.ToLower(strings.TrimSpace(k.key))}]
		if reservedOwners[k.key] || len(names) == 0 {
			continue
		}
		r := OwnerRemap{Tenant: k.tenant, From: k.key}
		for n := range names {
			r.Accounts = append(r.Accounts, n)
		}
		sort.Strings(r.Accounts)
		if len(r.Accounts) == 1 && policy == OwnersLocalOnly {
			r.To = auth.LocalOwner(r.Accounts[0])
		} else {
			r.To, r.Ambiguous = UnclaimedPrefix+k.key, true
		}
		tag, err := tx.Exec(ctx, `UPDATE sessions SET user_id = $1 WHERE tenant_id = $2 AND user_id = $3`,
			r.To, k.tenant, k.key)
		if err != nil {
			return nil, fmt.Errorf("owner migration: move %q: %w", k.key, err)
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

// foldEmailOwners lowercases every session owner that is a plain email with
// auth.FoldEmailOwner, the fold a caller's owner gets, in every tenant. It is
// done here rather than in SQL so the two folds agree on every collation.
func foldEmailOwners(ctx context.Context, tx pgx.Tx) ([]EmailFold, error) {
	if _, err := tx.Exec(ctx, `ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY`); err != nil {
		return nil, fmt.Errorf("owner migration: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT user_id FROM sessions
		 WHERE strpos(user_id, '@') > 0 AND strpos(user_id, ':') = 0`)
	if err != nil {
		return nil, fmt.Errorf("owner migration: read email owners: %w", err)
	}
	groups := map[string][]string{} // folded → every spelling the rows had
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		f := auth.FoldEmailOwner(k)
		groups[f] = append(groups[f], k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []EmailFold
	for to, variants := range groups {
		sort.Strings(variants)
		f := EmailFold{To: to, Variants: variants}
		for _, v := range variants {
			if v == to {
				continue
			}
			tag, err := tx.Exec(ctx, `UPDATE sessions SET user_id = $1 WHERE user_id = $2`, to, v)
			if err != nil {
				return nil, fmt.Errorf("owner migration: lowercase %q: %w", v, err)
			}
			f.Sessions += tag.RowsAffected()
		}
		if f.Sessions > 0 {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].To < out[j].To })
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
			slog.Warn("sessions left unclaimed: their old owner names a local account, and it is not certain the account wrote them",
				"tenant", r.Tenant, "old_owner", r.From, "accounts", strings.Join(r.Accounts, ","),
				"sessions", r.Sessions, "now", r.To)
			continue
		}
		slog.Info("sessions moved to their account", "tenant", r.Tenant, "old_owner", r.From, "owner", r.To, "sessions", r.Sessions)
	}
}

// errOwnersNotMigrated is Open's answer for a database the version 4 move has
// not run on: serving it would show every account an empty history.
var errOwnersNotMigrated = errors.New("the database schema is older than this server: " +
	"session owners have not been migrated (schema version 4). " +
	"Run `abhed migrate` as the owner (see docs/guide/02-configuration.md)")
