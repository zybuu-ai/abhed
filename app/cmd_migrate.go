package app

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store"
)

// migrateCmd applies the schema as the owning role and grants the runtime role
// what the server needs. It is the one place the owner's credentials are used.
func migrateCmd(workspace string, extensions []store.Extension, trust config.TrustChoice) int {
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust})
	if err == nil {
		err = cfg.Workspace.DeploymentError("migrate")
	}
	if err != nil {
		fail(err)
	}
	if cfg.Storage.Driver != "postgres" {
		fmt.Fprintln(os.Stderr, "abhed: storage.driver is not postgres; there is nothing to migrate")
		return 2
	}
	if cfg.Storage.MigrateDSN == "" {
		fmt.Fprintln(os.Stderr, "abhed: no owner connection. Set ABHED_MIGRATE_DATABASE_URL (or storage.migrate_dsn) to the role\n"+
			"that should own the tables. It must differ from the role in storage.dsn, which the server runs as.")
		return 2
	}
	runtime, err := pgx.ParseConfig(cfg.Storage.DSN)
	if err != nil {
		fail(fmt.Errorf("storage.dsn: %w", err))
	}
	if err := store.Provision(context.Background(), store.ProvisionConfig{
		OwnerDSN: cfg.Storage.MigrateDSN, RuntimeRole: runtime.User, Extensions: extensions,
	}); err != nil {
		fail(err)
	}
	fmt.Printf("Schema applied. %q may insert and read events and cannot change or remove them.\n"+
		"Keep the owner's credentials off the host that runs the server.\n", runtime.User)
	return 0
}

// printStoreStatus reports what the connection found, not what the config
// hoped for: how much is stored, and whether this role could alter it.
func printStoreStatus(pg *store.Postgres) {
	if sessions, events, err := pg.Stats(context.Background()); err == nil {
		fmt.Printf("            %d sessions · %d events persisted\n", sessions, events)
	}
	if pg.RecordProtected() {
		fmt.Println("            record protected — this role cannot change or remove events")
		return
	}
	fmt.Println("            record NOT protected from this server's credentials (storage.single_role)")
}
