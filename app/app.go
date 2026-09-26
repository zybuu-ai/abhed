// Package app is the abhed command: every subcommand, the wiring that turns
// a config into a running server, and the seams an edition extends it at.
//
// The binary used to be one main package, which is the right shape for a
// program with one edition and the wrong shape the moment there are two. An
// edition adds an identity provider, a scheduler, an exporter, a page; it
// does not want a second copy of two thousand lines of wiring to keep in
// step with the first. So the wiring is here, once, and an edition is a main
// that calls Main with options. The options are the whole of the contract:
// what an edition can add is what this file lets it add.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/docsite"
	"github.com/zybuu-ai/abhed/internal/sandboxconfig"
	"github.com/zybuu-ai/abhed/internal/tools"
	abhed "github.com/zybuu-ai/abhed/sdk"
	"github.com/zybuu-ai/abhed/server"
	"github.com/zybuu-ai/abhed/store"
)

// App is the binary's registry: what it can authenticate with, which paid
// features it carries, and what runs around the server. Built from Options
// by Main and read-only after that.
type App struct {
	version string
	edition string

	auth       map[string]AuthBuilder
	features   map[string]bool
	commands   map[string]Command
	migrate    []store.Extension
	taps       []TapBuilder
	serverOpts []ServerOptionsHook
	serveHooks []ServeHook
}

// Option configures the App that Main builds.
type Option func(*App)

func newApp(opts ...Option) *App {
	a := &App{
		version:  "dev",
		edition:  "Community Edition",
		auth:     map[string]AuthBuilder{},
		features: map[string]bool{},
		commands: map[string]Command{},
	}
	WithAuthMode("local", buildLocal)(a)
	for _, o := range opts {
		o(a)
	}
	return a
}

// WithVersion sets the version the binary reports. The linker sets it on
// the main package, so the main package passes it here.
func WithVersion(v string) Option { return func(a *App) { a.version = v } }

// WithEdition names the edition, for the version line and for the message
// that refuses a config this binary cannot honour.
func WithEdition(name string) Option { return func(a *App) { a.edition = name } }

// WithDocsURL names where this documentation is also published, for a build
// that did not embed it. The binary never links there itself: an air-gapped
// install has no route to it, and the console's air-gap guard forbids an
// external URL in the shipped HTML. It is printed once, at startup, for the
// operator reading the terminal.
func WithDocsURL(u string) Option { return func(*App) { docsite.PublicURL = u } }

// AuthBuilder constructs the provider and verifier for one auth.mode. Either
// may be nil: proxy mode has neither, an API-only identity provider has only
// a verifier. users is the account store, opened only when local accounts
// are configured; a builder for another mode receives nil.
type AuthBuilder func(ctx context.Context, cfg config.Config, workspace string,
	users auth.UserStore) (auth.Provider, auth.TokenVerifier, error)

// WithAuthMode registers a builder for an auth.mode. The registry is what
// makes a mode admissible: a config naming a mode with no builder is refused
// at startup, by name, rather than silently served without authentication.
// "local" is always registered; "proxy" and "none" need no builder because
// there is nothing to build.
func WithAuthMode(mode string, b AuthBuilder) Option {
	return func(a *App) { a.auth[mode] = b }
}

// WithFeature declares that a paid feature is compiled in. The config keys
// for every feature exist in every edition — one schema, so a config moves
// between editions unchanged — and a key for a feature this binary lacks is
// refused at startup with a message naming the key, the tier and the fix,
// instead of a feature quietly not happening. This is the declaration that
// switches that refusal off.
// WithMigrateExtension adds schema that `abhed migrate` applies as the owner,
// with the privileges the runtime role gets on it. An edition with tables of
// its own registers them here, so one command provisions the whole record.
func WithMigrateExtension(ext store.Extension) Option {
	return func(a *App) { a.migrate = append(a.migrate, ext) }
}

func WithFeature(name string) Option {
	return func(a *App) { a.features[name] = true }
}

// Command is an extra subcommand. It receives the resolved workspace and the
// arguments after its name, and returns the exit code.
type Command func(workspace string, args []string) int

// WithCommand adds a subcommand. A name that is already a built-in is
// ignored: the built-ins are the product, not a default to override.
func WithCommand(name string, run Command) Option {
	return func(a *App) {
		if _, builtin := builtinCommands[name]; !builtin {
			a.commands[name] = run
		}
	}
}

// TapBuilder constructs an event tap for a deployment, or returns a nil tap
// when the config does not ask for one. It takes the config because a tap
// that needs an endpoint cannot exist before Main has loaded it; stop, when
// not nil, is called at shutdown.
type TapBuilder func(cfg config.Config) (tap func(abhed.Event), stop func(), err error)

// WithEventTap adds a tap on the event stream. Every tap sees every event as
// it is appended, on the appending goroutine, so a tap must return at once;
// several taps are fanned in, each unaware of the others.
func WithEventTap(b TapBuilder) Option {
	return func(a *App) { a.taps = append(a.taps, b) }
}

// ServerOptionsHook adjusts the server's options after the app has filled
// them and before the server is built: to mount routes, set the admin page,
// or supply an invite redeemer. The store and the auth middleware are
// already in place, so a hook can build on them.
type ServerOptionsHook func(cfg config.Config, o *server.Options) error

// WithServerOptions registers a hook on the server's options.
func WithServerOptions(h ServerOptionsHook) Option {
	return func(a *App) { a.serverOpts = append(a.serverOpts, h) }
}

// ServeHook runs after the server is built and before it listens, with the
// context that ends at shutdown. It is where something that needs the
// running server — a scheduler that starts sessions on it — is bound and
// started. stop, when not nil, runs at shutdown. An error here is a startup
// refusal.
type ServeHook func(ctx context.Context, s *server.Server, cfg config.Config) (stop func(), err error)

// OnServe registers a hook on `abhed serve`.
func OnServe(h ServeHook) Option {
	return func(a *App) { a.serveHooks = append(a.serveHooks, h) }
}

// subcommands are the ones Main dispatches itself, as its usage lists them.
var subcommands = []struct{ name, about string }{
	{"serve", "run the server: the console, the workbench and the API (-addr)"},
	{"init", "write a starter .abhed/config.json in the workspace"},
	{"doctor", "check the configuration, the model endpoint and the sandbox"},
	{"providers", "list the model provider types this build supports"},
	{"user", "manage local accounts: add, list, passwd, remove, import"},
	{"secret", "manage stored secrets: set, list, rm"},
	{"hawkeye", "report on a session, from its id or an exported events file"},
	{"migrate", "apply the database schema as the owning role"},
	{"resolve", "work on a forge issue in its own branch and open a pull request"},
	{"acp", "speak the Agent Client Protocol on stdio, for editors"},
	{"rpc", "take line-delimited JSON requests on stdin, answer on stdout"},
	{"index", "build the workspace's search index ahead of a session"},
	{"eval", "run the evaluation corpus against the configured model"},
	{"version", "print the version and exit"},
}

// builtinCommands are the subcommands an edition cannot replace.
var builtinCommands = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range subcommands {
		m[c.name] = true
	}
	return m
}()

// paidFeatures maps each gated feature to the tier that includes it.
var paidFeatures = map[string]string{
	"oidc":      "Team",
	"schedules": "Team",
	"telemetry": "Enterprise",
}

// EditionError is a config key this binary cannot honour.
type EditionError struct {
	Key     string // the key, as the operator wrote it
	Feature string
	Edition string
}

func (e *EditionError) Error() string {
	return fmt.Sprintf("%s is a %s feature and is not in this %s binary. "+
		"Remove the key to run here, or run the edition that includes it "+
		"(README: Editions)", e.Key, paidFeatures[e.Feature], e.Edition)
}

// checkEdition refuses a config that names a feature this binary lacks.
//
// Why refuse rather than ignore: mergeFile unmarshals onto the struct and
// encoding/json ignores nothing it recognises, so every key is accepted by
// every edition. Removing the keys from one edition would make a Team config
// on a Community binary parse cleanly and run with the feature absent —
// schedules that never fire, traces that never leave. And "unknown
// auth.mode" for oidc would read as a typo, not an edition boundary. Validate
// is unchanged, so `abhed doctor` still validates the whole config and then
// says which edition it needs.
func (a *App) checkEdition(cfg config.Config) error {
	if cfg.Auth.Mode == "oidc" && a.auth["oidc"] == nil {
		return &EditionError{Key: `auth.mode "oidc"`, Feature: "oidc", Edition: a.edition}
	}
	// Local accounts plus a named provider is the "password OR Google"
	// deployment, which is OIDC with a form beside it.
	if cfg.Auth.Mode == "local" && cfg.Auth.Provider != "" && cfg.Auth.ClientID != "" &&
		a.auth["oidc"] == nil {
		return &EditionError{Key: fmt.Sprintf("auth.provider %q with auth.client_id", cfg.Auth.Provider),
			Feature: "oidc", Edition: a.edition}
	}
	if cfg.Telemetry.Enabled && !a.features["telemetry"] {
		return &EditionError{Key: "telemetry.enabled", Feature: "telemetry", Edition: a.edition}
	}
	if len(cfg.Schedules) > 0 && !a.features["schedules"] {
		return &EditionError{Key: "schedules", Feature: "schedules", Edition: a.edition}
	}
	return nil
}

// loadConfig loads and validates the config, then checks it against this
// binary's edition. Used by the commands that would stand up the surfaces
// the paid keys configure; the CLI loop does not, because none of those keys
// change what a terminal session does.
func (a *App) loadConfig(workspace string) (config.Config, error) {
	cfg, err := config.Load(workspace)
	if err != nil {
		return cfg, err
	}
	registerState(cfg, workspace)
	return cfg, a.checkEdition(cfg)
}

// registerState marks the state files a configuration moves out of .abhed,
// so the agent's tools and the server's readers refuse them there too.
func registerState(cfg config.Config, workspace string) {
	for _, p := range sandboxconfig.StatePaths(cfg, workspace) {
		tools.AddStatePath(p)
	}
}

// buildLocal is the Community identity mechanism: accounts Abhed holds
// itself, for a deployment with no identity provider.
func buildLocal(_ context.Context, cfg config.Config, _ string, users auth.UserStore) (auth.Provider, auth.TokenVerifier, error) {
	ttl := time.Duration(cfg.Auth.SessionHours) * time.Hour
	return auth.NewLocalAuth(users, ttl, cfg.Auth.CookieSecure), nil, nil
}
