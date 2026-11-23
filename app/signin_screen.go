package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/identity"
	"github.com/zybuu-ai/abhed/profiles"
)

// A choice on the screen.
type whoChoice struct {
	key, label string
	do         func() (identity.Principal, error)
}

// chooseWho is the start's question, before anything else: sign in, create
// an account, sign in to a server, or guest, as the rules allow.
func chooseWho(s *identity.Store, rules identity.Rules, p *prompter) (identity.Principal, error) {
	list, err := s.List()
	if err != nil {
		return identity.Principal{}, err
	}
	var accounts, servers []identity.Entry
	for _, e := range list {
		switch {
		case e.Kind == identity.KindAccount && rules.SignIn != config.SignInServer:
			accounts = append(accounts, e)
		case e.Kind == identity.KindServer && rules.Admits(e) == nil:
			servers = append(servers, e)
		}
	}
	_, taken, _ := s.LegacyOwner()
	legacy := !taken && identity.LegacyData()

	var choices []whoChoice
	add := func(label string, do func() (identity.Principal, error)) {
		choices = append(choices, whoChoice{key: fmt.Sprint(len(choices) + 1), label: label, do: do})
	}
	if rules.SignIn != config.SignInServer {
		if legacy {
			add("Create my account and bring my data   (recommended; nothing is deleted)", func() (identity.Principal, error) {
				return createAndAdopt(s, p)
			})
		}
		if len(accounts) > 0 {
			add("Sign in            an account on this machine", func() (identity.Principal, error) {
				e, err := signInAccount(s, p, onlyName(accounts))
				if err != nil {
					return identity.Principal{}, err
				}
				return s.Principal(e), nil
			})
		}
		if !legacy {
			add("Create an account  on this machine; nothing leaves it", func() (identity.Principal, error) {
				e, code, err := createAccount(s, p, "", "", "", true)
				if err != nil {
					return identity.Principal{}, err
				}
				fmt.Fprintf(p.out, "Created %s. Recovery code (shown once; keep it somewhere safe): %s\n", e.Display(), code)
				return s.Principal(e), nil
			})
		}
	}
	for _, e := range servers {
		add("Sign in to "+e.Display(), func() (identity.Principal, error) { return serverSignIn(s, p, e.URL, rules) })
	}
	serverLabel := "Sign in to a server your own or your organisation's Abhed server"
	if rules.SignIn == config.SignInServer {
		serverLabel = "Sign in to " + rules.Server
	}
	add(serverLabel, func() (identity.Principal, error) {
		url := rules.Server
		if rules.SignIn != config.SignInServer {
			var err error
			if url, err = p.line("Server address", rules.Server); err != nil {
				return identity.Principal{}, err
			}
		}
		return serverSignIn(s, p, url, rules)
	})
	if rules.GuestAllowed {
		label := "Guest              nothing is kept when you quit"
		if legacy {
			label = "Guest for now      your earlier data stays as it is; asked again next time"
		}
		add(label, func() (identity.Principal, error) { return identity.GuestPrincipal(), nil })
	}

	fmt.Fprintln(p.out)
	if legacy {
		fmt.Fprintln(p.out, "Abhed now asks who is working. You have sessions, history or memory from")
		fmt.Fprintln(p.out, "earlier versions; create your account and they move into it.")
	} else {
		fmt.Fprintln(p.out, "Abhed — who is working?")
	}
	if rules.SignIn != config.SignInOptional {
		fmt.Fprintln(p.out, "Your organisation requires a sign-in on this machine.")
	}
	fmt.Fprintln(p.out)
	for _, c := range choices {
		fmt.Fprintf(p.out, "  %s) %s\n", c.key, c.label)
	}
	for range 5 {
		pick, err := p.line("\nChoose", "1")
		if err != nil {
			return identity.Principal{}, err
		}
		var chosen *whoChoice
		for i := range choices {
			if choices[i].key == pick {
				chosen = &choices[i]
			}
		}
		if chosen == nil {
			fmt.Fprintf(p.out, "  %q is not one of the choices.\n", pick)
			continue
		}
		pr, err := chosen.do()
		if err == nil {
			return pr, nil
		}
		if errors.Is(err, errNoInput) {
			return identity.Principal{}, err
		}
		fmt.Fprintf(p.out, "  %v\n", err)
	}
	return identity.Principal{}, errors.New("no choice made")
}

func onlyName(list []identity.Entry) string {
	if len(list) == 1 {
		return list[0].Username
	}
	return ""
}

// createAndAdopt makes the first account and moves the data from before
// sign-in into it.
func createAndAdopt(s *identity.Store, p *prompter) (identity.Principal, error) {
	// Remembered only once the data has moved: until then no other start may
	// resolve to the account and run on half of it.
	e, code, err := createAccount(s, p, "", "", "", false)
	if err != nil {
		return identity.Principal{}, err
	}
	fmt.Fprintf(p.out, "Created %s. Recovery code (shown once; keep it somewhere safe): %s\n", e.Display(), code)
	res, err := identity.Migrate(s, e, legacyUser())
	if err != nil {
		return identity.Principal{}, fmt.Errorf("your account was made, but the earlier data did not move (%w); nothing was lost, and abhed account adopt tries again", err)
	}
	if err := s.Remember(e.ID); err != nil {
		return identity.Principal{}, err
	}
	fmt.Fprintf(p.out, "Moved %d item(s) into your account; %d session record(s) verified before and after.\n", res.Moved, res.Sessions)
	return s.Principal(e), nil
}

// serverSignIn signs in to url with abhed login, keeping the profile in the
// server sign-in's own folder, and makes it the principal.
func serverSignIn(s *identity.Store, p *prompter, url string, rules identity.Rules) (identity.Principal, error) {
	base, err := profiles.NormalizeURL(strings.TrimSpace(url), false)
	if err != nil {
		return identity.Principal{}, err
	}
	return signInViaLogin(s, rules, []string{base}, p.out, p.out, openBrowser)
}

// loginAsPrincipal is abhed login on a machine no one is signed in to: the
// server sign-in it makes is who works here from now on.
func loginAsPrincipal(args []string, out, errOut io.Writer) int {
	s, err := identity.Open()
	if err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1
	}
	rules, err := clientRules()
	if err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1
	}
	pr, err := signInViaLogin(s, rules, args, out, errOut, openBrowser)
	if err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "This machine now starts as %s, until abhed account signout.\n", pr.Display())
	return 0
}

// signInViaLogin runs abhed login with args into a file of its own, then
// keeps the profile in the server sign-in's folder and remembers it.
func signInViaLogin(s *identity.Store, rules identity.Rules, args []string, out, errOut io.Writer, open func(string) error) (identity.Principal, error) {
	pend, err := serverLogin(context.Background(), s, args, out, errOut, open)
	if err != nil {
		return identity.Principal{}, err
	}
	defer pend.discard()
	return pend.file(s, rules)
}

// pendingSignIn is a server sign-in made but not yet kept: its token waits
// in a file of its own until file keeps it, or discard drops it.
type pendingServerSignIn struct {
	path  string
	filed *bool
}

// discard removes the pending file. A sign-in that was never kept (cancelled,
// or refused by the rules) also has its token ended on the server, so it does
// not stay valid there with no one holding it.
func (p pendingServerSignIn) discard() {
	if p.filed != nil && !*p.filed {
		if reg, err := profiles.Load(p.path); err == nil {
			if _, prof, err := reg.Get(""); err == nil && prof.Token != "" {
				if err := endToken(prof); err != nil {
					fmt.Fprintf(os.Stderr, "abhed: the server sign-in that was not kept could not be ended at %s (%v); its token stays valid there until it expires\n", prof.URL, err)
				}
			}
		}
	}
	_ = os.Remove(p.path)
}

// endToken tells p's server to end its token, retrying as abhed logout does.
func endToken(p *profiles.Profile) error {
	c, err := p.HTTPClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return profiles.Logout(ctx, c, p, logoutAttempts, logoutBackoff)
}

// serverLogin is the network part of a server sign-in: abhed login, into a
// file of this sign-in's own, so two at once never share one.
func serverLogin(ctx context.Context, s *identity.Store, args []string, out, errOut io.Writer, open func(string) error) (pendingServerSignIn, error) {
	f, err := os.CreateTemp(s.Dir(), ".pending-profiles-*.json")
	if err != nil {
		return pendingServerSignIn{}, err
	}
	pending := pendingServerSignIn{path: f.Name(), filed: new(bool)}
	_ = f.Close()
	_ = os.Remove(pending.path) // abhed login makes it, 0600
	if code := loginWith("", args, out, errOut, loginOpts{open: open, file: pending.path, ctx: ctx}); code != 0 {
		pending.discard()
		if ctx.Err() != nil {
			return pendingServerSignIn{}, errors.New("the server sign-in was cancelled")
		}
		return pendingServerSignIn{}, errors.New("the server sign-in did not complete")
	}
	return pending, nil
}

// file keeps a pending server sign-in: checked against the rules first, so
// a refused server leaves no entry, then filed in the sign-in's own folder
// and remembered.
func (p pendingServerSignIn) file(s *identity.Store, rules identity.Rules) (identity.Principal, error) {
	reg, err := profiles.Load(p.path)
	if err != nil {
		return identity.Principal{}, err
	}
	name, prof, err := reg.Get("")
	if err != nil {
		return identity.Principal{}, err
	}
	if err := rules.Admits(identity.Entry{Kind: identity.KindServer, URL: prof.URL}); err != nil {
		return identity.Principal{}, err
	}
	e, err := s.AddServer(prof.URL, prof.Username, prof.Display, name)
	if err != nil {
		return identity.Principal{}, err
	}
	root, err := s.Root(e)
	if err != nil {
		return identity.Principal{}, err
	}
	dest := filepath.Join(root, "profiles.json")
	have, err := profiles.Load(dest)
	if err != nil {
		return identity.Principal{}, err
	}
	// A profile of the same name signed in before is replaced: its token is
	// ended, or kept as a pending sign-out that later commands retry.
	if old := have.Profiles[name]; old != nil && old.Token != "" && old.Token != prof.Token {
		have.SignOut(name, endToken(old))
	}
	have.Profiles[name] = prof
	have.Current = name
	if err := profiles.Save(dest, have); err != nil {
		return identity.Principal{}, err
	}
	// The token is kept in the sign-in's folder now; it is not ended.
	*p.filed = true
	if err := s.Remember(e.ID); err != nil {
		return identity.Principal{}, err
	}
	return s.Principal(e), nil
}

// legacyUser is who the record wrote sessions as before sign-in.
func legacyUser() string { return legacyCLIUser() }
