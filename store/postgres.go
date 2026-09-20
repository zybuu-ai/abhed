// Package store provides durable persistence for Abhed's event stream.
//
// The in-memory store is fine for a CLI session; it is not fine for audit. This
// package makes sessions survive restart and gives compliance a substrate it can
// rely on: append-only events enforced by database trigger, tenant isolation
// enforced by row-level security, and deterministic replay (docs P6, §10).
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zybuu-ai/abhed/internal/agent"
)

//go:embed schema.sql
var schemaSQL string

// Postgres implements agent.Store durably.
type Postgres struct {
	pool   *pgxpool.Pool
	tenant string

	// Subscribers receive events for live streaming. The database is the
	// durable record; this is the notification path for SSE.
	mu   sync.RWMutex
	subs map[string][]chan agent.Event

	// The accounts table is created on first use rather than at Open, so an
	// OIDC deployment never creates a table it will not use. Once, because
	// the user operations all call MigrateUsers defensively and one of them
	// is on the sign-in path.
	usersOnce sync.Once
	usersErr  error
}

type Config struct {
	DSN string
	// Tenant scopes every statement via app.tenant_id, which row-level
	// security then enforces.
	Tenant           string
	MaxConns         int32
	ConnectTimeout   time.Duration
	StatementTimeout time.Duration
}

func DefaultConfig(dsn string) Config {
	return Config{
		DSN:              dsn,
		Tenant:           "default",
		MaxConns:         10,
		ConnectTimeout:   10 * time.Second,
		StatementTimeout: 30 * time.Second,
	}
}

// Open connects and applies the schema.
func Open(ctx context.Context, cfg Config) (*Postgres, error) {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 10
	}
	if cfg.Tenant == "" {
		cfg.Tenant = "default"
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	// Every pooled connection carries the tenant, so RLS applies even to a
	// query that forgets its WHERE clause.
	tenant := cfg.Tenant
	poolCfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SELECT set_config('app.tenant_id', $1, false)", tenant)
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %s: %w", redactDSN(cfg.DSN), err)
	}

	// Row-level security is the tenant boundary, and Postgres does not apply it
	// to a superuser or a BYPASSRLS role — not even with FORCE. A deployment
	// connected that way has every isolation policy in the schema and none of
	// the isolation. Refusing to start is the only honest response: a control
	// that is silently off is worse than one that is visibly missing.
	var privileged bool
	if err := pool.QueryRow(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&privileged); err != nil {
		pool.Close()
		return nil, fmt.Errorf("check role privileges: %w", err)
	}
	if privileged {
		pool.Close()
		return nil, fmt.Errorf("refusing to run as a superuser or BYPASSRLS role: " +
			"row-level security would be bypassed and tenants would not be isolated. " +
			"Connect as a plain role that owns the tables " +
			"(see docs/guide/02-configuration.md)")
	}

	p := &Postgres{pool: pool, tenant: cfg.Tenant, subs: make(map[string][]chan agent.Event)}
	if err := p.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Migrate applies the schema. Idempotent, so it is safe on every start —
// which matters for an air-gapped install where a separate migration step is
// one more thing to get wrong.
func (p *Postgres) Migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

func (p *Postgres) Close() { p.pool.Close() }

// CreateSession records a session before its first event. Events reference
// sessions, so this must happen first.
func (p *Postgres) CreateSession(ctx context.Context, s SessionRecord) error {
	if s.Tenant == "" {
		s.Tenant = p.tenant
	}
	// The pooled connection sets app.tenant_id from the store's configured
	// tenant, and row-level security enforces that every written row matches
	// it. A caller passing a different tenant is therefore not a row RLS should
	// silently drop — it is a configuration error, and saying so is far more
	// useful than "new row violates row-level security policy".
	if s.Tenant != p.tenant {
		return fmt.Errorf(
			"cannot write session for tenant %q: this store is scoped to tenant %q. "+
				"Set storage.tenant to match the tenant your requests carry, or run a "+
				"store per tenant", s.Tenant, p.tenant)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO sessions (id, tenant_id, user_id, workspace, model,
		                      prompt_hash, harness_version, mode, parent_id, started_at, prompt)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11)
		ON CONFLICT (id) DO NOTHING`,
		s.ID, s.Tenant, s.User, s.Workspace, s.Model,
		s.PromptHash, s.HarnessVersion, s.Mode, s.ParentID, s.StartedAt,
		truncatePrompt(s.Prompt))
	if err != nil {
		return fmt.Errorf("create session %s: %w", s.ID, err)
	}
	return nil
}

type SessionRecord struct {
	ID             string
	Tenant         string
	User           string
	Workspace      string
	Model          string
	PromptHash     string
	HarnessVersion string
	Mode           string
	// Prompt is the opening request, kept so a session list is readable. It is
	// truncated on write: the list needs a label, not a transcript.
	Prompt    string
	ParentID  string
	StartedAt time.Time

	EndedAt        *time.Time
	TerminalReason string
	Turns          int
	TokensIn       int64
	TokensOut      int64
	TokensCached   int64
	Compactions    int
	// Nil when the session predates these columns or the adapter reports no
	// window. See the note in schema.sql: absent is not the same as zero.
	ContextTokens *int64
	ContextWindow *int64
}

// CreateSubSession records a subagent's session row. Subagents are sessions in
// their own right, so their events need a parent row like any other.
func (p *Postgres) CreateSubSession(ctx context.Context, id, description string) error {
	return p.CreateSession(ctx, SessionRecord{
		ID: id, Tenant: p.tenant, User: "agent",
		Workspace: description, Model: "subagent", Mode: "auto",
		StartedAt: time.Now().UTC(),
	})
}

// Append persists one event. It satisfies agent.Store.
//
// A duplicate (session_id, seq) is treated as success rather than an error:
// replaying an append after a crash must be safe, and the unique constraint is
// what makes that idempotent.
func (p *Postgres) Append(ev agent.Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := p.pool.Exec(ctx, `
		INSERT INTO events (id, session_id, tenant_id, parent_id, seq, type,
		                    payload, actor, trust, created_at)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10)
		ON CONFLICT (session_id, seq) DO NOTHING`,
		ev.ID, ev.SessionID, p.tenant, ev.ParentID, ev.Seq, string(ev.Type),
		[]byte(ev.Payload), string(ev.Actor), string(ev.Trust), ev.CreatedAt)
	if err != nil {
		// A missing session FK is the common misuse; say so plainly.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return fmt.Errorf("append event for unknown session %s: call CreateSession first", ev.SessionID)
		}
		return fmt.Errorf("append event %s/%d: %w", ev.SessionID, ev.Seq, err)
	}

	p.publish(ev)

	// Session totals are derived from the terminal event, so a crashed session
	// still shows whatever it accumulated.
	if ev.Type == agent.EvSessionEnded {
		p.finalizeSession(ctx, ev)
	}
	return nil
}

func (p *Postgres) finalizeSession(ctx context.Context, ev agent.Event) {
	var ended agent.SessionEnded
	if err := jsonUnmarshal(ev.Payload, &ended); err != nil {
		return
	}
	// Written as NULL when the loop could not measure, so "not recorded" stays
	// distinguishable from "the context was empty". Zero is a real reading for
	// neither.
	var ctxTokens, ctxWindow *int64
	if ended.ContextTokens > 0 {
		v := int64(ended.ContextTokens)
		ctxTokens = &v
	}
	if ended.ContextWindow > 0 {
		v := int64(ended.ContextWindow)
		ctxWindow = &v
	}
	_, _ = p.pool.Exec(ctx, `
		UPDATE sessions SET ended_at = $2, terminal_reason = $3, turns = $4,
		       tokens_in = $5, tokens_out = $6, tokens_cached = $7, compactions = $8,
		       context_tokens = $9, context_window = $10
		WHERE id = $1`,
		ev.SessionID, ev.CreatedAt, string(ended.Reason), ended.Turns,
		ended.TokensIn, ended.TokensOut, ended.TokensCached, ended.Compactions,
		ctxTokens, ctxWindow)
}

// truncatePrompt bounds what goes in the label column.
func truncatePrompt(s string) string {
	const max = 300
	if len(s) > max {
		end := max
		for !utf8.RuneStart(s[end]) {
			end--
		}
		return s[:end] + "…"
	}
	return s
}

func (p *Postgres) Events(sessionID string) ([]agent.Event, error) {
	return p.Since(sessionID, 0)
}

// Since returns events after seq, which is what Last-Event-ID resumption needs.
func (p *Postgres) Since(sessionID string, seq int64) ([]agent.Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := p.pool.Query(ctx, `
		SELECT id, session_id, COALESCE(parent_id,''), seq, type, payload, actor, trust, created_at
		FROM events
		WHERE session_id = $1 AND seq > $2
		  AND NOT EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND deleted_at IS NOT NULL)
		ORDER BY seq`, sessionID, seq)
	if err != nil {
		return nil, fmt.Errorf("query events for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []agent.Event
	for rows.Next() {
		var ev agent.Event
		var payload []byte
		var typ, actor, trust string
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.ParentID, &ev.Seq,
			&typ, &payload, &actor, &trust, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.Type = agent.EventType(typ)
		ev.Actor = agent.Actor(actor)
		ev.Trust = agent.Trust(trust)
		ev.Payload = payload
		out = append(out, ev)
	}
	return out, rows.Err()
}

// ListSessions returns recent sessions for the current tenant. RLS restricts
// the result even if this query were wrong.
func (p *Postgres) ListSessions(ctx context.Context, limit int) ([]SessionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := p.pool.Query(ctx, `
		SELECT id, tenant_id, user_id, workspace, model, mode, COALESCE(prompt,''),
		       started_at, ended_at, COALESCE(terminal_reason,''),
		       turns, tokens_in, tokens_out, tokens_cached, compactions,
		       context_tokens, context_window
		FROM sessions WHERE deleted_at IS NULL ORDER BY started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionRecord
	for rows.Next() {
		var s SessionRecord
		if err := rows.Scan(&s.ID, &s.Tenant, &s.User, &s.Workspace, &s.Model, &s.Mode, &s.Prompt,
			&s.StartedAt, &s.EndedAt, &s.TerminalReason,
			&s.Turns, &s.TokensIn, &s.TokensOut, &s.TokensCached, &s.Compactions,
			&s.ContextTokens, &s.ContextWindow); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (p *Postgres) GetSession(ctx context.Context, id string) (SessionRecord, error) {
	var s SessionRecord
	err := p.pool.QueryRow(ctx, `
		SELECT id, tenant_id, user_id, workspace, model, mode, COALESCE(prompt,''),
		       started_at, ended_at, COALESCE(terminal_reason,''),
		       turns, tokens_in, tokens_out, tokens_cached, compactions,
		       context_tokens, context_window
		FROM sessions WHERE id = $1 AND deleted_at IS NULL`, id).Scan(
		&s.ID, &s.Tenant, &s.User, &s.Workspace, &s.Model, &s.Mode, &s.Prompt,
		&s.StartedAt, &s.EndedAt, &s.TerminalReason,
		&s.Turns, &s.TokensIn, &s.TokensOut, &s.TokensCached, &s.Compactions,
		&s.ContextTokens, &s.ContextWindow)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

var ErrNotFound = errors.New("not found")

// ClaimResume marks a finished session as running again, atomically, so
// that only one process continues it. It reports false when the session is
// unknown, deleted, or already running — including when another node claimed
// it a moment ago, which is the case this exists for: two replicas behind a
// load balancer must not both continue the same conversation.
func (p *Postgres) ClaimResume(ctx context.Context, sessionID string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE sessions SET ended_at = NULL, terminal_reason = NULL
		WHERE id = $1 AND ended_at IS NOT NULL AND deleted_at IS NULL`, sessionID)
	if err != nil {
		return false, fmt.Errorf("claim session %s: %w", sessionID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteSession marks a session deleted. The transcript rows stay — the events
// table refuses DELETE by trigger, and that refusal is a property the
// deployment promised — but Events, ListSessions and GetSession all treat a
// marked session as absent from then on, which is what the person who pressed
// delete needed: nothing reads it through the API again. The marking is not
// a second delete path around the audit log; it is the audit log recording
// that a delete happened, and by whom.
//
// Deleting a session that is already deleted, or that belongs to another
// tenant (RLS makes it invisible), is not an error: the caller asked for a
// state and that is the state.
func (p *Postgres) DeleteSession(sessionID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := p.pool.Exec(ctx, `
		UPDATE sessions SET deleted_at = now(), deleted_by = user_id
		WHERE id = $1 AND deleted_at IS NULL`, sessionID)
	if err != nil {
		return fmt.Errorf("delete session %s: %w", sessionID, err)
	}
	return nil
}

// SaveCheckpoint records a file's prior content, backing /undo.
func (p *Postgres) SaveCheckpoint(ctx context.Context, sessionID string, seq int64, path string, before []byte) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO checkpoints (id, session_id, tenant_id, event_seq, path, before)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		fmt.Sprintf("%s-%d", sessionID, seq), sessionID, p.tenant, seq, path, before)
	return err
}

// Subscribe streams newly appended events for a session.
func (p *Postgres) Subscribe(sessionID string) <-chan agent.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := make(chan agent.Event, 256)
	p.subs[sessionID] = append(p.subs[sessionID], ch)
	return ch
}

func (p *Postgres) Unsubscribe(sessionID string, ch <-chan agent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	subs := p.subs[sessionID]
	for i, c := range subs {
		if c == ch {
			p.subs[sessionID] = append(subs[:i], subs[i+1:]...)
			close(c)
			return
		}
	}
}

func (p *Postgres) publish(ev agent.Event) {
	p.mu.RLock()
	subs := append([]chan agent.Event(nil), p.subs[ev.SessionID]...)
	p.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // never block the agent loop on a slow consumer
		}
	}
}

// Scan streams every event committed in [from, to), oldest first, to fn,
// stopping at the first error fn returns. It sees only the store's own
// tenant: the pooled connection carries app.tenant_id and row-level security
// applies to this query as to any other.
//
// This is the seam an audit export or a retention job stands on. Neither
// belongs in the store — what is exported, in what format, signed how, is an
// edition's decision — but both need the events in commit order and both need
// to be sure they are not reading across a tenant, and those two properties
// are the store's to guarantee.
func (p *Postgres) Scan(ctx context.Context, from, to time.Time, fn func(agent.Event) error) error {
	rows, err := p.pool.Query(ctx, `
		SELECT id, session_id, COALESCE(parent_id,''), seq, type, payload, actor, trust, created_at
		FROM events
		WHERE created_at >= $1 AND created_at < $2
		ORDER BY created_at, session_id, seq`, from, to)
	if err != nil {
		return fmt.Errorf("scan events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ev agent.Event
		var payload []byte
		var typ, actor, trust string
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.ParentID, &ev.Seq,
			&typ, &payload, &actor, &trust, &ev.CreatedAt); err != nil {
			return err
		}
		ev.Type = agent.EventType(typ)
		ev.Actor = agent.Actor(actor)
		ev.Trust = agent.Trust(trust)
		ev.Payload = payload
		if err := fn(ev); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Pool exposes the connection pool so an edition can keep tables of its own
// beside the audit log — in the same database, under the same tenant setting,
// with one thing to back up. Every connection has already run set_config for
// the tenant, so a table that enables row-level security on tenant_id gets
// the same isolation the events table has without doing anything further.
func (p *Postgres) Pool() *pgxpool.Pool { return p.pool }

// Tenant is the tenant this store is scoped to.
func (p *Postgres) Tenant() string { return p.tenant }

// Stats reports counts for `abhed doctor`.
func (p *Postgres) Stats(ctx context.Context) (sessions, events int64, err error) {
	err = p.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM sessions), (SELECT count(*) FROM events)`).
		Scan(&sessions, &events)
	return
}

// redactDSN removes the password before a DSN reaches a log or error message.
func redactDSN(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			if colon := strings.Index(rest[:at], ":"); colon >= 0 {
				return dsn[:i+3] + rest[:colon] + ":***" + rest[at:]
			}
		}
	}
	return dsn
}
