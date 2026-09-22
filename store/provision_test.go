package store

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// singleRoleConfig is the setup the older tests were written for: one role
// that owns the tables and applies the schema itself.
func singleRoleConfig(dsn string) Config {
	cfg := DefaultConfig(dsn)
	cfg.SingleRole = true
	return cfg
}

// runtimeStore provisions as the owner (ABHED_TEST_DSN) and opens as the
// runtime role (ABHED_TEST_RUNTIME_DSN), which is how a deployment should run.
func runtimeStore(t *testing.T, tenant string) *Postgres {
	t.Helper()
	owner, runtime := testDSN(t), os.Getenv("ABHED_TEST_RUNTIME_DSN")
	if runtime == "" {
		t.Skip("set ABHED_TEST_RUNTIME_DSN to a second, unprivileged role to run the privilege-separation tests")
	}
	rc, err := pgx.ParseConfig(runtime)
	if err != nil {
		t.Fatalf("parse runtime dsn: %v", err)
	}
	if err := Provision(context.Background(), ProvisionConfig{OwnerDSN: owner, RuntimeRole: rc.User}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg := DefaultConfig(runtime)
	cfg.Tenant = tenant
	p, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open as the runtime role: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// The claim is that the record cannot be altered with the server's own
// credentials. Every route found to do it is tried here as the runtime role;
// the claim stands on all of them failing and the rows still being there.
func TestRuntimeRoleCannotAlterTheRecord(t *testing.T) {
	p := runtimeStore(t, "t-sep")
	if !p.RecordProtected() {
		t.Fatal("the runtime role opened, but the store does not consider the record protected")
	}
	ctx := context.Background()
	id := testID(t, "sess-sep-")
	newSession(t, p, id, "t-sep")
	for seq := int64(1); seq <= 2; seq++ {
		if err := p.Append(ev(id, seq, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "deploy to prod"})); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	attempts := map[string]string{
		"rewrite an event":             `UPDATE events SET payload = '{"text":"nothing happened"}' WHERE session_id = '` + id + `'`,
		"delete an event":              `DELETE FROM events WHERE session_id = '` + id + `'`,
		"truncate the record":          `TRUNCATE events`,
		"truncate with cascade":        `TRUNCATE sessions CASCADE`,
		"disable the update trigger":   `ALTER TABLE events DISABLE TRIGGER events_no_update`,
		"disable every trigger":        `ALTER TABLE events DISABLE TRIGGER ALL`,
		"drop the trigger":             `DROP TRIGGER events_no_update ON events`,
		"replace the trigger function": `CREATE OR REPLACE FUNCTION abhed_events_immutable() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql`,
		"skip triggers for a session":  `SET session_replication_role = replica`,
		"switch off row security":      `ALTER TABLE events DISABLE ROW LEVEL SECURITY`,
		"take ownership":               `ALTER TABLE events OWNER TO CURRENT_USER`,
		"drop the table":               `DROP TABLE events CASCADE`,
	}
	for what, sql := range attempts {
		if _, err := p.pool.Exec(ctx, sql); err == nil {
			t.Errorf("the runtime role was able to %s: %s", what, sql)
		}
	}
	// Granting itself a privilege does not error: Postgres warns that nothing
	// was granted and carries on. So this one is judged by its outcome — and
	// the same question is the last word on everything above.
	_, _ = p.pool.Exec(ctx, `GRANT UPDATE, DELETE, TRUNCATE ON events TO CURRENT_USER`)
	if how, err := recordExposure(ctx, p.pool); err != nil || how != "" {
		t.Fatalf("after the attempts the runtime role can alter the record: %q (err %v)", how, err)
	}

	events, err := p.Events(id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("the record has %d events after the attempts, want 2", len(events))
	}
	for _, e := range events {
		if !strings.Contains(string(e.Payload), "deploy to prod") {
			t.Fatalf("event #%d was rewritten: %s", e.Seq, e.Payload)
		}
	}
}

// Grants that are too tight are a production outage. Everything the server
// legitimately does has to work as the runtime role.
func TestRuntimeRoleCanDoItsJob(t *testing.T) {
	p := runtimeStore(t, "t-job")
	ctx := context.Background()
	id := testID(t, "sess-job-")
	newSession(t, p, id, "t-job")

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s failed as the runtime role — a grant is missing: %v", what, err)
		}
	}
	must("append", p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "hi"})))
	must("append session end", p.Append(ev(id, 2, agent.EvSessionEnded, agent.Trusted,
		agent.SessionEnded{Reason: agent.TermCompleted, Turns: 1})))
	_, err := p.Events(id)
	must("read events", err)
	_, err = p.ListSessions(ctx, 10)
	must("list sessions", err)
	must("claim node", p.ClaimNode(ctx, id, "node-a"))
	must("release node", p.ReleaseNode(ctx, id, "node-a"))
	must("save checkpoint", p.SaveCheckpoint(ctx, id, 1, "a.go", []byte("before")))
	apID, err := p.AskApproval(ctx, Approval{SessionID: id, Tool: "bash"})
	must("ask approval", err)
	_, err = p.AnswerApproval(ctx, apID, true, "reviewer")
	must("answer approval", err)
	if ok, err := p.ClaimResume(ctx, id); err != nil || !ok {
		t.Fatalf("claim resume = %v, %v", ok, err)
	}

	name := testID(t, "u-")
	must("put user", p.Put(ctx, &auth.User{Username: name, Tenant: "t-job", Hash: "x"}))
	_, err = p.Get(ctx, name)
	must("get user", err)
	_, err = p.List(ctx)
	must("list users", err)
	must("delete user", p.Delete(ctx, name))
	must("soft-delete session", p.DeleteSession(id))
}

// A connection that owns the record is refused unless the operator has said,
// in configuration, that they accept what that means.
func TestOpenRefusesARoleThatCanAlterTheRecord(t *testing.T) {
	dsn := testDSN(t)
	openStore(t, "t-refuse") // make sure the schema exists, as its owner

	_, err := Open(context.Background(), DefaultConfig(dsn))
	if err == nil {
		t.Fatal("a role that owns the events table was accepted without storage.single_role")
	}
	for _, want := range []string{"owns the events table", "abhed migrate", "single_role"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

// TRUNCATE fires no row trigger. It removed every event in one statement
// until it got a trigger of its own; this holds even for the owning role.
func TestTruncateIsRefused(t *testing.T) {
	p := openStore(t, "t-trunc")
	id := testID(t, "sess-trunc-")
	newSession(t, p, id, "t-trunc")
	if err := p.Append(ev(id, 1, agent.EvUserMessage, agent.Trusted, agent.Message{Text: "keep me"})); err != nil {
		t.Fatal(err)
	}
	_, err := p.pool.Exec(context.Background(), `TRUNCATE events`)
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("TRUNCATE events = %v, want the append-only refusal", err)
	}
	if evs, _ := p.Events(id); len(evs) != 1 {
		t.Fatalf("%d events survive, want 1", len(evs))
	}
}

func TestProvisionRefusesOneRoleForBothJobs(t *testing.T) {
	dsn := testDSN(t)
	c, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	err = Provision(context.Background(), ProvisionConfig{OwnerDSN: dsn, RuntimeRole: c.User})
	if err == nil || !strings.Contains(err.Error(), "must not own the record") {
		t.Fatalf("provisioning one role as both owner and runtime = %v", err)
	}
}

// Privilege lists are spliced into SQL, so only the four keywords get through.
func TestValidPrivileges(t *testing.T) {
	for _, ok := range []string{"SELECT", "SELECT, INSERT", "select,insert,update,delete"} {
		if !validPrivileges(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "ALL", "TRUNCATE", "SELECT; DROP TABLE events", "SELECT ON events TO public --"} {
		if validPrivileges(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// An edition's own tables are provisioned by the same command, and the
// runtime role gets on them exactly the privileges the edition named.
func TestProvisionAppliesExtensions(t *testing.T) {
	owner, runtimeDSN := os.Getenv("ABHED_TEST_DSN"), os.Getenv("ABHED_TEST_RUNTIME_DSN")
	if owner == "" || runtimeDSN == "" {
		t.Skip("needs ABHED_TEST_DSN (owner) and ABHED_TEST_RUNTIME_DSN")
	}
	ctx := context.Background()
	rc, err := pgx.ParseConfig(runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	ext := Extension{
		SQL:    "CREATE TABLE IF NOT EXISTS ext_probe (id text PRIMARY KEY, n bigserial, note text)",
		Grants: map[string]string{"ext_probe": "SELECT, INSERT"},
	}
	if err := Provision(ctx, ProvisionConfig{OwnerDSN: owner, RuntimeRole: rc.User, Extensions: []Extension{ext}}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, runtimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "INSERT INTO ext_probe (id, note) VALUES ($1, 'x') ON CONFLICT DO NOTHING", testID(t, "ext-")); err != nil {
		t.Fatalf("the runtime role cannot insert into the extension's table: %v", err)
	}
	if _, err := conn.Exec(ctx, "DELETE FROM ext_probe"); err == nil {
		t.Fatal("the runtime role could delete from a table it was granted only select and insert on")
	}
}
