// Package identity answers who is working before anything else runs: an
// account on this machine, a sign-in to an Abhed server, or guest. The
// answer picks the person's folder (package home); policy, the sandbox,
// egress and approvals do not depend on it.
//
// See docs/architecture/sign-in.md.
package identity

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zybuu-ai/abhed/auth"
	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/home"
)

// Kind is what a principal is.
type Kind string

const (
	// Legacy is a machine with data from before sign-in that no account has
	// taken over, run as it always was, only where nothing asks for sign-in.
	Legacy  Kind = "legacy"
	Account Kind = "account"
	Server  Kind = "server"
	Guest   Kind = "guest"
)

// Owner namespaces for what principals record. Neither can equal a server's
// owner: package auth reserves both.
const (
	DevicePrefix = auth.DevicePrefix
	GuestOwner   = auth.Guest
)

// Principal is who is working.
type Principal struct {
	Kind Kind
	// Entry is the account or server sign-in, for those kinds.
	Entry Entry
	// Owner is who the record writes sessions as.
	Owner string
	// Root is the person's folder; "" is ~/.abhed (legacy).
	Root string
	// Unverified is a server sign-in whose server could not be reached at
	// start to confirm it.
	Unverified bool
}

// Display is who the principal is, to a person.
func (p Principal) Display() string {
	switch p.Kind {
	case Account, Server:
		return p.Entry.Display()
	case Guest:
		return "guest"
	}
	return "this machine's user (sign-in not set up)"
}

// Rules are what the managed configuration and the org policy say about
// sign-in.
type Rules struct {
	SignIn       string
	Server       string
	GuestAllowed bool
	// GuestOff is why guest is not allowed, when it is not.
	GuestOff     string
	RememberDays *int
}

// RulesOf reads the rules from cfg. Where the organisation ships every
// session's record, a guest, who holds no credential to ship it with, is
// refused rather than run unshipped.
func RulesOf(cfg config.Config) Rules {
	r := Rules{SignIn: cfg.Auth.Client.SignIn, Server: cfg.SignInServer(),
		GuestAllowed: cfg.Auth.Client.GuestAllowed(), RememberDays: cfg.Auth.Client.RememberDays}
	switch {
	case !r.GuestAllowed:
		r.GuestOff = "your organisation has turned guest off"
	case cfg.Record.Ship.Enabled && cfg.ManagedSets("record.ship"):
		r.GuestAllowed = false
		r.GuestOff = "your organisation ships every session's record, which needs a sign-in"
	}
	return r
}

func (r Rules) guestOff() string {
	if r.GuestOff != "" {
		return r.GuestOff
	}
	return "your organisation has turned guest off"
}

// Admits reports why the rules refuse e, or nil.
func (r Rules) Admits(e Entry) error {
	switch r.SignIn {
	case config.SignInServer:
		if e.Kind != KindServer || NormalServerURL(e.URL) != NormalServerURL(r.Server) {
			return fmt.Errorf("your organisation requires a sign-in to %s", r.Server)
		}
	}
	return nil
}

// ErrSignInRequired is a run that needs a sign-in it does not have; the CLI
// exits ExitSignInRequired.
var ErrSignInRequired = errors.New("sign-in required")

// ExitSignInRequired is the exit code of a run refused for want of sign-in.
const ExitSignInRequired = 10

// RequiredError says why a sign-in is required and what to run.
type RequiredError struct{ Why, Next string }

func (e *RequiredError) Error() string {
	if e.Next == "" {
		return "sign-in required: " + e.Why
	}
	return "sign-in required: " + e.Why + "; " + e.Next
}

func (e *RequiredError) Unwrap() error { return ErrSignInRequired }

// Request is what a start knows about who is working.
type Request struct {
	// Guest is -guest or ABHED_GUEST=1.
	Guest bool
	// Account is -account or ABHED_ACCOUNT: an account's username, or a
	// server sign-in's username@host.
	Account string
	// Interactive is a terminal or a client that can show the choice.
	Interactive bool
	Rules       Rules
}

// Outcome is the answer, or that the person must be asked.
type Outcome struct {
	Principal Principal
	// Ask is set when the person must choose; Principal is empty then.
	Ask bool
	// Note is one line for stderr, for an answer nobody chose.
	Note string
}

// LegacyData reports whether ~/.abhed holds a person's work from before
// sign-in: sessions an earlier version recorded, or its prompt history.
// Other items alone (trust.json, a plugin) are not enough, since a command
// a 1.3 binary runs before anyone signs in could write them; what is there
// still moves with the rest when an account takes the data over.
func LegacyData() bool {
	m, err := home.Machine()
	if err != nil {
		return false
	}
	for _, p := range legacyWork {
		if nonEmpty(filepath.Join(m, p)) {
			return true
		}
	}
	return false
}

// LegacyItemsPresent reports whether any of LegacyItems is in ~/.abhed, for
// abhed account adopt, which the person asks for by name.
func LegacyItemsPresent() bool {
	m, err := home.Machine()
	if err != nil {
		return false
	}
	for _, p := range LegacyItems {
		if _, err := os.Lstat(filepath.Join(m, p)); err == nil {
			return true
		}
	}
	return false
}

// legacyWork are the items only a session writes.
var legacyWork = []string{"records", "history"}

// nonEmpty reports whether p is a file, or a folder with something in it.
func nonEmpty(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return fi.Mode().IsRegular() && fi.Size() > 0
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	names, _ := f.Readdirnames(1)
	return len(names) > 0
}

// LegacyItems are the person's state in ~/.abhed before sign-in, which the
// first account takes over.
var LegacyItems = []string{"records", "history", "drafts", "projects", "ABHED.md", "secrets.json",
	"profiles.json", "trust.json", "command-trust.json", "startup-files.json", "exports", "push", "index",
	"ui.json", "skills", "agents", "commands", "styles", "plugins", "plugins.json", "plugin-trusted-root.json",
	filepath.Join("state", "workspace-grants.json"), filepath.Join("state", "record-ship.json"),
	filepath.Join("state", "debug-process-tier.json")}

// Resolve answers who is working, in the order the design gives: an
// explicit choice, a remembered sign-in, the person (when they can be
// asked), then, for a run nobody can ask, the data from before sign-in or
// guest, unless the rules require a sign-in.
func Resolve(s *Store, r Request) (Outcome, error) {
	if r.Guest {
		if !r.Rules.GuestAllowed {
			return Outcome{}, &RequiredError{Why: r.Rules.guestOff(), Next: signInHint(r.Rules)}
		}
		return Outcome{Principal: GuestPrincipal()}, nil
	}
	if r.Account != "" {
		e, err := lookup(s, r.Account)
		if err != nil {
			return Outcome{}, err
		}
		if err := r.Rules.Admits(e); err != nil {
			return Outcome{}, &RequiredError{Why: err.Error(), Next: signInHint(r.Rules)}
		}
		if rem, ok, err := s.Remembered(r.Rules.RememberDays); err != nil {
			return Outcome{}, err
		} else if ok && rem.ID == e.ID {
			return Outcome{Principal: s.principal(rem)}, nil
		}
		if r.Interactive {
			return Outcome{Ask: true}, nil
		}
		return Outcome{}, &RequiredError{Why: e.Display() + " is not signed in on this machine",
			Next: "run abhed account signin " + e.Username + " in a terminal"}
	}
	if rem, ok, err := s.Remembered(r.Rules.RememberDays); err != nil {
		return Outcome{}, err
	} else if ok && r.Rules.Admits(rem) == nil {
		return Outcome{Principal: s.principal(rem)}, nil
	}
	if r.Interactive {
		return Outcome{Ask: true}, nil
	}
	required := r.Rules.SignIn != config.SignInOptional
	if !required {
		if _, taken, err := s.LegacyOwner(); err == nil && !taken && LegacyData() {
			return Outcome{Principal: Principal{Kind: Legacy, Owner: ""},
				Note: "abhed: sign-in is not set up; running as before. Start abhed in a terminal once to create your account."}, nil
		}
	}
	if required || !r.Rules.GuestAllowed {
		why := "your organisation requires it"
		if !required {
			why = "no one is signed in, and " + r.Rules.guestOff()
		}
		return Outcome{}, &RequiredError{Why: why, Next: signInHint(r.Rules)}
	}
	return Outcome{Principal: GuestPrincipal(),
		Note: "abhed: no sign-in; running as guest, nothing is kept (pass -guest to silence this, or sign in once in a terminal)"}, nil
}

func signInHint(r Rules) string {
	if r.SignIn == config.SignInServer {
		return "run abhed login " + r.Server + " in a terminal"
	}
	return "run abhed in a terminal to sign in"
}

// lookup finds -account's entry: an account's username, or username@host
// for a server sign-in.
func lookup(s *Store, name string) (Entry, error) {
	list, err := s.List()
	if err != nil {
		return Entry{}, err
	}
	for _, e := range list {
		if e.Kind == KindAccount && strings.EqualFold(e.Username, name) {
			return e, nil
		}
	}
	for _, e := range list {
		if e.Kind == KindServer && (strings.EqualFold(e.Username+"@"+hostOf(e.URL), name) || strings.EqualFold(e.Email, name)) {
			return e, nil
		}
	}
	return Entry{}, &RequiredError{Why: fmt.Sprintf("there is no account %q on this machine", name),
		Next: "abhed account list shows them"}
}

// principal is e as the principal working.
func (s *Store) principal(e Entry) Principal {
	p := Principal{Kind: Account, Entry: e, Root: filepath.Join(s.dir, e.ID)}
	if e.Kind == KindServer {
		p.Kind = Server
	}
	p.Owner = OwnerOf(e)
	return p
}

// Principal is e as the principal working.
func (s *Store) Principal(e Entry) Principal { return s.principal(e) }

// OwnerOf is who the record writes e's sessions as on this machine.
func OwnerOf(e Entry) string {
	if e.Kind == KindServer {
		return DevicePrefix + strings.ToLower(e.Username) + "@" + hostOf(e.URL)
	}
	return DevicePrefix + strings.ToLower(e.Username)
}

// GuestPrincipal is guest; its folder is made by Enter.
func GuestPrincipal() Principal { return Principal{Kind: Guest, Owner: GuestOwner} }

// NormalServerURL is a server's URL as sign-ins compare it: scheme and host
// lowercased, the scheme's default port and a trailing slash dropped.
func NormalServerURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.TrimSpace(raw), "/")
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if p := u.Port(); (u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80") {
		u.Host = u.Hostname()
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}
