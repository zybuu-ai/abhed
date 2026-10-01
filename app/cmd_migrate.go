package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/store"
)

// migrateCmd applies the schema as the owning role and grants the runtime role
// what the server needs. It is the one place the owner's credentials are used.
func migrateCmd(workspace string, args []string, extensions []store.Extension, trust config.TrustChoice) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	owners := fs.String("owners", "", "what to do with sessions keyed by a local account's name or email: "+
		"local-only (move them to the account) or unclaim; default from auth.mode")
	noAccounts := fs.Bool("force-no-accounts", false, "run a local-only owner migration that finds no accounts")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: abhed migrate [--owners=local-only|unclaim] [--force-no-accounts]")
		return 2
	}
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
	policy := ownerPolicy(cfg)
	if *owners != "" {
		if policy, err = store.ParseOwnerPolicy(*owners); err != nil {
			fail(err)
		}
	}
	fmt.Printf("Session owners: %s (%s).\n", policy, ownerPolicyWhy(cfg, *owners != ""))
	fileAccounts, filePath, err := fileOwnerAccounts(cfg, workspace, *noAccounts)
	if err != nil {
		fail(err)
	}
	err = provision(context.Background(), store.ProvisionConfig{
		OwnerDSN: cfg.Storage.MigrateDSN, RuntimeRole: runtime.User, Extensions: extensions,
		Owners: policy, OwnerAccounts: fileAccounts, AllowNoAccounts: *noAccounts,
		AccountsFound: func(table, extra, distinct int) {
			from := fmt.Sprintf("%d in the users table", table)
			if filePath != "" {
				from += fmt.Sprintf(", %d in %s", extra, filePath)
			}
			fmt.Printf("Local accounts for the owner migration: %d (%s).\n", distinct, from)
			if distinct == 0 && policy == store.OwnersLocalOnly {
				fmt.Fprintln(os.Stderr, "abhed: WARNING: no local accounts found; sessions under old owners cannot be moved to anyone")
			}
		},
	})
	if errors.Is(err, store.ErrNoOwnerAccounts) {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if err != nil {
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
