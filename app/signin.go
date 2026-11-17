package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/home"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/profiles"
)

// Who is working, for this process: set once by the gate before anything
// else runs (docs/architecture/sign-in.md).
var (
	principalMu sync.Mutex
	principal   = identity.Principal{Kind: identity.Legacy}
	guestNow    *identity.GuestSession
	// byDefault is set when nobody chose the principal: a run no one could
	// ask, with no sign-in held.
	byDefault bool
)

// currentPrincipal is who is working.
func currentPrincipal() identity.Principal {
	principalMu.Lock()
	defer principalMu.Unlock()
	return principal
}

// unresolvedIsLegacy makes a run nobody chose a principal for run as before
// sign-in, rather than asking or running as guest: the package's tests,
// which predate sign-in, keep their records. Never set outside tests.
var unresolvedIsLegacy = false

// EnvGuest and EnvAccount choose guest, or an account, as the flags do.
const (
	EnvGuest   = "ABHED_GUEST"
	EnvAccount = "ABHED_ACCOUNT"
)

// personCommands are the subcommands that read or write a person's state, and
// so run as whoever is working. The rest (serve, account, pki, admin…) are the
// machine's.
var personCommands = map[string]bool{
	"record": true, "logs": true, "sessions": true, "hawkeye": true, "mcp": true, "secret": true,
	"trust": true, "resolve": true, "index": true, "login": true, "logout": true, "profile": true,
	"bg": true, "policy": true, "acp": true, "rpc": true,
}

// signInFlags are what the command line says about who is working.
type signInFlags struct {
	guest   bool
	account string
}

func (s signInFlags) request(interactive bool, rules identity.Rules) identity.Request {
	req := identity.Request{Guest: s.guest, Account: s.account, Interactive: interactive, Rules: rules}
	if !req.Guest && os.Getenv(EnvGuest) == "1" {
		req.Guest = true
	}
	if req.Account == "" {
		req.Account = strings.TrimSpace(os.Getenv(EnvAccount))
	}
	return req
}

// terminalBoth reports whether stdin and stderr are a terminal, so a person
// can be asked.
func terminalBoth() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// clientRules are the sign-in rules of the managed configuration and the
// cached org policy, which bind every principal on the machine.
func clientRules() (identity.Rules, error) {
	m, err := config.LoadManagedWith(config.LoadOptions{Warn: func(string) {}})
	if err != nil {
		return identity.Rules{}, fmt.Errorf("the managed configuration does not load: %w", err)
	}
	return identity.RulesOf(m), nil
}

// enterPrincipal answers who is working and makes their folder the
// person's, or returns the exit code to stop with: 10 when a sign-in is
// required and none is held. A process an Abhed started inherits its
// parent's answer.
func enterPrincipal(sf signInFlags, interactive bool, in *os.File, errOut io.Writer) int {
	code, err := resolvePrincipal(sf, interactive, in, errOut)
	if err != nil {
		return refusedSignIn(err, errOut)
	}
	return code
}

// resolvePrincipal is enterPrincipal returning a refusal to sign in as an
// error, for the caller to report.
func resolvePrincipal(sf signInFlags, interactive bool, in *os.File, errOut io.Writer) (int, error) {
	if removed, err := identity.SweepGuests(); err != nil {
		fmt.Fprintf(errOut, "abhed: removing what earlier guests left: %v\n", err)
	} else if len(removed) > 0 && interactive {
		fmt.Fprintf(errOut, "abhed: removed %d guest folder(s) an earlier run left behind\n", len(removed))
	}
	rules, err := clientRules()
	if err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1, nil
	}
	s, err := identity.Open()
	if err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1, nil
	}
	// Never resolve while the earlier data is being moved: wait a little for
	// another abhed's move, finish one a crash left, else stop.
	if resumed, err := identity.SettleMigration(s, migrationWait); err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1, nil
	} else if resumed {
		fmt.Fprintln(errOut, "abhed: finished moving your earlier data into your account")
	}
	out, err := identity.Resolve(s, sf.request(interactive, rules))
	if err != nil {
		return 0, err
	}
	if out.Principal.Kind == identity.Server {
		verified, err := serverStillAdmits(s, rules, out.Principal)
		switch {
		case errors.Is(err, errNoServerCredential) && rules.SignIn == "":
			// Not a sign-in at all: forgotten, and the start goes on as if there
			// had been none.
			if out, err = identity.Resolve(s, sf.request(interactive, rules)); err != nil {
				return 0, err
			}
		case err != nil:
			return 0, err
		default:
			out.Principal.Unverified = !verified
		}
	}
	p := out.Principal
	if unresolvedIsLegacy && (out.Ask || p.Kind == identity.Guest && out.Note != "") {
		p, out.Ask, out.Note = identity.Principal{Kind: identity.Legacy}, false, ""
	}
	if out.Ask {
		if p, err = chooseWho(s, rules, newPrompter(in, errOut)); err != nil {
			return 0, err
		}
	}
	if err := usePrincipal(p); err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1, nil
	}
	if out.Note != "" {
		principalMu.Lock()
		byDefault = true
		principalMu.Unlock()
		fmt.Fprintln(errOut, out.Note)
	}
	return 0, nil
}

// guestByDefault reports whether this process is guest only because no one
// is signed in, not because anyone chose it.
func guestByDefault() bool {
	principalMu.Lock()
	defer principalMu.Unlock()
	return byDefault && principal.Kind == identity.Guest
}

// pending is why a protocol client (rpc, acp) cannot start yet: a sign-in
// is required and none is held. It asks over the protocol instead.
var pending error

func pendingSignIn() error {
	principalMu.Lock()
	defer principalMu.Unlock()
	return pending
}

func setPendingSignIn(err error) {
	principalMu.Lock()
	pending = err
	principalMu.Unlock()
}

// enterPrincipalForClient is enterPrincipal for rpc and acp, whose client can
// sign the person in over the protocol: a required sign-in it does not have
// waits for that instead of ending the process.
func enterPrincipalForClient(sf signInFlags, errOut io.Writer) int {
	code, err := resolvePrincipal(sf, false, os.Stdin, errOut)
	if errors.Is(err, identity.ErrSignInRequired) {
		setPendingSignIn(err)
		return 0
	}
	if err != nil {
		return refusedSignIn(err, errOut)
	}
	return code
}

func refusedSignIn(err error, errOut io.Writer) int {
	fmt.Fprintf(errOut, "abhed: %v\n", err)
	if errors.Is(err, identity.ErrSignInRequired) {
		return identity.ExitSignInRequired
	}
	return 1
}

// usePrincipal makes p the person working in this process.
func usePrincipal(p identity.Principal) error {
	switch p.Kind {
	case identity.Guest:
		g, err := identity.EnterGuest()
		if err != nil {
			return fmt.Errorf("make the guest folder: %w", err)
		}
		p.Root = g.Root
		// Each guest is its own owner: no guest reads another's sessions where
		// an organisation keeps them.
		p.Owner = identity.GuestOwner + ":" + filepath.Base(g.Root)
		principalMu.Lock()
		guestNow = g
		principalMu.Unlock()
	case identity.Account, identity.Server:
		if err := home.SetPerson(p.Root); err != nil {
			return err
		}
	}
	principalMu.Lock()
	principal = p
	principalMu.Unlock()
	handDown(p)
	return nil
}

// handDown tells the Abhed a hook, an MCP server or a command starts who is
// working here, in the selectors that can only narrow: ABHED_GUEST for a
// guest, ABHED_ACCOUNT for an account or server sign-in, which still needs
// that sign-in remembered. A guest's process also drops the variables that
// would point it at an account's server sign-ins or secrets.
func handDown(p identity.Principal) {
	_ = os.Unsetenv(EnvGuest)
	_ = os.Unsetenv(EnvAccount)
	switch p.Kind {
	case identity.Guest:
		_ = os.Setenv(EnvGuest, "1")
		_ = os.Unsetenv(config.ProfilesEnv)
		_ = os.Unsetenv(secrets.EnvFile)
	case identity.Account:
		_ = os.Setenv(EnvAccount, p.Entry.Username)
	case identity.Server:
		_ = os.Setenv(EnvAccount, p.Entry.Username+"@"+hostOfURL(p.Entry.URL))
	}
}

// hostOfURL is a URL's host[:port], as -account names a server sign-in.
func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}

// endGuest removes the guest's folder; nothing of a guest is kept. The
// process is no one's after it.
func endGuest() {
	principalMu.Lock()
	g := guestNow
	guestNow = nil
	if principal.Kind != identity.Legacy {
		principal = identity.Principal{Kind: identity.Legacy}
		_ = home.SetPerson("")
		_ = os.Unsetenv(EnvGuest)
		_ = os.Unsetenv(EnvAccount)
	}
	byDefault = false
	principalMu.Unlock()
	if g != nil {
		if err := g.End(); err != nil {
			fmt.Fprintf(os.Stderr, "abhed: the guest folder could not all be removed (%v); the next start removes the rest\n", err)
		}
	}
}

// guestRefusal is the refusal of something a guest cannot do because it
// keeps state past the process, or "".
func guestRefusal(what string) error {
	if currentPrincipal().Kind != identity.Guest {
		return nil
	}
	return fmt.Errorf("%s keeps state after this run, and a guest keeps nothing; sign in to do it (abhed account signin)", what)
}

// guestSubcommand refuses, to a guest, a subcommand that would keep state
// past the run or change the machine's configuration: a stored secret, an
// MCP server or its sign-in, a server sign-in. It returns the refusal or nil.
func guestSubcommand(rest []string) error {
	verb := ""
	if len(rest) > 1 {
		verb = rest[1]
	}
	what := ""
	switch rest[0] {
	case "secret":
		if verb == "set" || verb == "rm" || verb == "remove" {
			what = "a stored secret"
		}
	case "mcp":
		switch verb {
		case "add", "remove", "rm":
			what = "an MCP server in this machine's configuration"
		case "login":
			what = "an MCP server's sign-in"
		}
	case "login", "logout":
		what = "a server sign-in"
	case "profile":
		if verb == "use" || verb == "rm" || verb == "remove" {
			what = "a server sign-in"
		}
	}
	if what == "" {
		return nil
	}
	return guestRefusal(what)
}

// whoLine is the start's line about who is working, and how to switch.
func whoLine(p identity.Principal) string {
	switch p.Kind {
	case identity.Guest:
		return "Guest: nothing is kept when you quit. /account to sign in."
	case identity.Legacy:
		return "Sign-in is not set up yet; Abhed works as before. /account to create your account."
	}
	return "Signed in as " + p.Display() + ". /account to switch."
}

// migrationWait is how long a start waits for another abhed's move of the
// data from before sign-in.
var migrationWait = 30 * time.Second

// serverCheckWait bounds the check of a server sign-in at start, so an
// offline machine starts on what it remembers.
var serverCheckWait = 3 * time.Second

// errNoServerCredential is a remembered server sign-in that is no sign-in:
// nothing to sign in with, or a token the server refused.
var errNoServerCredential = errors.New("no credential")

// noCredential is a RequiredError that is also errNoServerCredential; it
// prints as the RequiredError alone.
type noCredential struct{ *identity.RequiredError }

func (e noCredential) Is(target error) bool { return target == errNoServerCredential }
func (e noCredential) Unwrap() error        { return e.RequiredError }

// serverStillAdmits asks a remembered server sign-in's server whether it
// still admits the person, and reports whether it could ask.
//
// A sign-in ends only for an answer that says so: the organisation removed
// the person's access, or the server refused the token (401 or 403, after
// the refresh Do already tries), or there is nothing to sign in with. Any
// other answer (5xx, 429, anything unexpected) or no answer at all is the
// server being unavailable: the sign-in stays and the run goes on
// unverified, but under sign_in "server" only while the stored token is
// still within its life. The person's data on this machine is always kept.
func serverStillAdmits(s *identity.Store, rules identity.Rules, p identity.Principal) (verified bool, err error) {
	gone := func(why string) (bool, error) {
		_, _ = s.ForgetIf(p.Entry.ID)
		return false, noCredential{&identity.RequiredError{Why: why, Next: "run abhed login " + p.Entry.URL + " in a terminal"}}
	}
	reg, err := profiles.Load(filepath.Join(p.Root, "profiles.json"))
	if err != nil {
		return gone("the sign-in to " + p.Entry.URL + " cannot be read on this machine")
	}
	_, prof, err := reg.Get(p.Entry.Profile)
	if err != nil || prof.Token == "" {
		return gone("the sign-in to " + p.Entry.URL + " holds no credential on this machine")
	}
	c, err := prof.HTTPClient()
	if err != nil {
		return gone("the sign-in to " + p.Entry.URL + " cannot be used: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), serverCheckWait)
	defer cancel()
	_, err = profiles.Whoami(ctx, c, prof)
	var removed *profiles.AccessRemovedError
	var refused *profiles.RefusedError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &removed):
		_, _ = s.ForgetIf(p.Entry.ID)
		return false, &identity.RequiredError{Why: "your organisation removed your access to " + p.Entry.URL +
			"; signing in again will not restore it, so ask its Abhed administrator (your data on this machine is kept)"}
	case errors.As(err, &refused) && refused.TokenRefused():
		return gone("the server " + p.Entry.URL + " no longer takes this sign-in (" + refused.Text + ")")
	}
	// Unavailable: kept, unverified.
	if rules.SignIn == config.SignInServer && !prof.SignedIn(time.Now()) {
		return false, &identity.RequiredError{Why: "the sign-in to " + p.Entry.URL + " has expired and the server cannot be reached to renew it",
			Next: "run abhed login " + p.Entry.URL + " in a terminal once it can be reached"}
	}
	return false, nil
}

// principalStart is who is working, as session.started records it, so the
// record, its OCSF copy and the org server each say whose session it was:
// a guest's included.
func principalStart() map[string]any {
	p := currentPrincipal()
	out := map[string]any{"kind": string(p.Kind)}
	if p.Kind == identity.Server {
		// false: the server could not be reached at start to confirm the sign-in.
		out["verified"] = !p.Unverified
	}
	if owner := cliUser(); owner != "" {
		out["owner"] = owner
	}
	if guestByDefault() {
		out["by_default"] = true
	}
	return out
}
