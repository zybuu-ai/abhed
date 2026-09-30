package app

import (
	"context"
	crand "crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
)

// splitPositional pulls the first non-flag argument out of a list, returning
// the remaining flags and that value. Needed because flag.Parse treats the
// first bare word as the end of the flags.
func splitPositional(args []string) (flags []string, positional string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if positional == "" && !strings.HasPrefix(a, "-") {
			// Not a flag value: the preceding token, if a flag, used "=" or
			// is boolean. Abhed's user flags all take values, so a bare word
			// following "-email" belongs to it.
			if i > 0 && strings.HasPrefix(args[i-1], "-") &&
				!strings.Contains(args[i-1], "=") {
				flags = append(flags, a)
				continue
			}
			positional = a
			continue
		}
		flags = append(flags, a)
	}
	return flags, positional
}

// userCmd manages local accounts: abhed user add | list | passwd | remove.
func userCmd(workspace string, args []string, trust config.TrustChoice) int {
	cfg, err := config.LoadWith(workspace, config.LoadOptions{Trust: trust})
	if err == nil {
		err = cfg.Workspace.DeploymentError("user")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if cfg.Auth.Mode != "local" {
		fmt.Fprintf(os.Stderr,
			"abhed: auth.mode is %q, so Abhed does not hold accounts.\n"+
				"Set \"auth\": {\"mode\": \"local\"} to manage users here.\n",
			orDefault(cfg.Auth.Mode, "none"))
		return 1
	}

	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	switch action {
	case "add", "passwd", "import":
		// The same check serve makes, so an account is not written to a file
		// the server will then refuse to start with.
		if err := sandboxconfig.CheckStatePaths(cfg, workspace); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
	}

	us, err := userStore(cfg, workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
		return 1
	}
	if fs, isFile := us.(*auth.FileUserStore); isFile {
		fmt.Fprintf(os.Stderr, "abhed: accounts in %s "+
			"(set storage.driver to postgres for a multi-node deployment)\n", fs.Path())
	}
	la := auth.NewLocalAuth(us, 0, cfg.Auth.CookieSecure)
	ctx := context.Background()

	switch action {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ExitOnError)
		email := fs.String("email", "", "email address")
		name := fs.String("name", "", "display name")
		tenant := fs.String("tenant", "", "tenant (defaults to storage.tenant)")
		groups := fs.String("groups", "", "comma-separated groups")
		admin := fs.Bool("admin", false, "grant administrator rights (settings, users, invites)")
		pass := fs.String("password", "", "password (generated if omitted)")

		// Go's flag package stops at the first non-flag argument, so parsing
		// "add demo -email x" would silently discard every flag after the
		// username. Lift the positional out first, then parse the rest.
		rest, username := splitPositional(args[1:])
		if username == "" {
			fmt.Fprintln(os.Stderr, "usage: abhed user add <username> [-email ...] [-name ...]")
			return 2
		}
		_ = fs.Parse(rest) // the set exits on a bad flag; nothing is left to check

		password := *pass
		if password == "" {
			password = generatePassword()
			fmt.Printf("generated password: %s\n", password)
			fmt.Println("  (change it after first sign-in)")
		}

		// A password an administrator chose or was shown is temporary: the
		// person changes it at first sign-in.
		u := auth.User{
			Username: username, Email: *email, Name: *name,
			Tenant:     orDefault(*tenant, orDefault(cfg.Storage.Tenant, "default")),
			MustChange: true,
		}
		if *groups != "" {
			u.Groups = splitRules(*groups)
		}
		// The admin group is what gates settings, users and invites. A
		// deployment whose first account is not an admin cannot reach its own
		// settings page, so this is offered as a flag rather than something to
		// discover from a config file.
		if *admin {
			g := cfg.Auth.AdminGroup
			if g == "" {
				g = server.DefaultAdminGroup
			}
			if !slices.Contains(u.Groups, g) {
				u.Groups = append(u.Groups, g)
			}
		}
		if err := la.CreateUser(ctx, u, password); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		fmt.Printf("created %s (tenant %s)\n", username, u.Tenant)
		fmt.Println("  must set a new password at first sign-in")
		if *admin {
			fmt.Println("  administrator — can manage settings and users")
		}

	case "list":
		users, err := us.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		if len(users) == 0 {
			fmt.Println("no accounts yet — create one with: abhed user add <username>")
			return 0
		}
		sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
		fmt.Printf("%-20s %-28s %-12s %s\n", "USERNAME", "EMAIL", "TENANT", "GROUPS")
		for _, u := range users {
			fmt.Printf("%-20s %-28s %-12s %s\n",
				u.Username, orDefault(u.Email, "—"), u.Tenant, strings.Join(u.Groups, ","))
		}

	case "passwd":
		fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
		pass := fs.String("password", "", "new password (generated if omitted)")
		// As for add: the flag may come before or after the username.
		rest, username := splitPositional(args[1:])
		if err := fs.Parse(rest); err != nil || username == "" || fs.NArg() > 0 {
			fmt.Fprintln(os.Stderr, "usage: abhed user passwd <username> [-password ...]")
			return 2
		}
		u, err := us.Get(ctx, username)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		password := *pass
		if password == "" {
			password = generatePassword()
		}
		if err := la.CreateUserOrReset(ctx, u, password); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		if *pass == "" {
			fmt.Printf("new password for %s: %s\n", u.Username, password)
		} else {
			fmt.Printf("password set for %s\n", u.Username)
		}
		fmt.Println("  must set a new password at first sign-in")

	case "remove", "rm":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: abhed user remove <username>")
			return 2
		}
		if err := us.Delete(ctx, args[1]); err != nil {
			if errors.Is(err, auth.ErrNoSuchUser) {
				fmt.Fprintf(os.Stderr, "abhed: no such user: %s\n", args[1])
			} else {
				fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			}
			return 1
		}
		fmt.Printf("removed %s\n", args[1])

	case "import":
		// Switching storage.driver from memory/file to postgres leaves every
		// existing account behind in the file, with no error and no hint —
		// the accounts simply are not there any more. This moves them.
		if cfg.Storage.Driver != "postgres" {
			fmt.Fprintln(os.Stderr,
				"abhed: import copies accounts INTO postgres; set storage.driver first")
			return 1
		}
		src, err := auth.NewFileUserStore(usersFile(cfg, workspace))
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: %v\n", err)
			return 1
		}
		accounts, err := src.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "abhed: read %s: %v\n", src.Path(), err)
			return 1
		}
		if len(accounts) == 0 {
			fmt.Printf("no accounts in %s\n", src.Path())
			return 0
		}
		moved, skipped := 0, 0
		for _, u := range accounts {
			// Never overwrite an account that already exists in the target:
			// a re-run of import must not clobber a password changed since.
			if existing, _ := us.Get(ctx, u.Username); existing != nil {
				fmt.Printf("  skip   %s (already in postgres)\n", u.Username)
				skipped++
				continue
			}
			if err := us.Put(ctx, u); err != nil {
				fmt.Fprintf(os.Stderr, "abhed: import %s: %v\n", u.Username, err)
				return 1
			}
			fmt.Printf("  import %s\n", u.Username)
			moved++
		}
		fmt.Printf("%d imported, %d already present\n", moved, skipped)
		if moved > 0 {
			// Left in place deliberately: it is the only copy of those hashes
			// until the operator is satisfied the move worked.
			fmt.Printf("%s is unchanged — delete it once you have signed in\n", src.Path())
		}

	default:
		fmt.Fprintln(os.Stderr, "usage: abhed user [add|list|passwd|remove|import]")
		return 2
	}
	return 0
}

// generatePassword produces a readable but strong initial password, so an
// operator can hand it over without inventing one that turns out to be weak.
func generatePassword() string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		panic("abhed: system random source unavailable: " + err.Error())
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out)
}

// userStore returns durable account storage when Postgres is configured, and
// in-memory otherwise. In-memory is fine for a pilot but says so at startup:
// accounts vanishing on restart should never be a surprise.
func userStore(cfg config.Config, workspace string) (auth.UserStore, error) {
	if cfg.Storage.Driver == "postgres" {
		pg, err := store.Open(context.Background(), storeConfig(cfg))
		if err != nil {
			return nil, fmt.Errorf("open user store: %w", err)
		}
		return pg, nil
	}
	// No Postgres: keep accounts in a file beside the workspace config, so
	// `abhed user add` and `abhed serve` see the same accounts. An in-memory
	// store here silently discarded every account the CLI created.
	return auth.NewFileUserStore(usersFile(cfg, workspace))
}

// usersFile is where local accounts live: the configured path, else beside
// the workspace config.
func usersFile(cfg config.Config, workspace string) string {
	if cfg.Auth.UsersFile != "" {
		return cfg.Auth.UsersFile
	}
	return filepath.Join(workspace, ".abhed", "users.json")
}
