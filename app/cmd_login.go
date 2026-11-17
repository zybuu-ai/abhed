package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/profiles"
)

// openBrowser opens a URL in the person's browser; replaced in tests. The
// person's BROWSER, when set, is used first.
var openBrowser = func(u string) error {
	if cmds := browserCommands(os.Getenv("BROWSER"), u); len(cmds) > 0 {
		var err error
		for _, c := range cmds {
			// A command the person chose, run with the URL as one argument; no shell.
			if err = exec.Command(c[0], c[1:]...).Start(); err == nil { // #nosec G204 -- the person's own BROWSER
				return nil
			}
		}
		return fmt.Errorf("BROWSER: %w", err)
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	// The URL is the server's own sign-in page, built here; no shell is involved.
	case "darwin":
		cmd = exec.Command("open", u) // #nosec G204 -- fixed program, URL as one argument
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u) // #nosec G204 -- fixed program, URL as one argument
	default:
		cmd = exec.Command("xdg-open", u) // #nosec G204 -- fixed program, URL as one argument
	}
	return cmd.Start()
}

// browserCommands reads BROWSER as other programs do: a list of commands
// separated by the path list separator, tried in turn, each given the URL in
// place of %s or after its words. Words are split on spaces; nothing is
// handed to a shell.
func browserCommands(env, u string) [][]string {
	var out [][]string
	for _, entry := range filepath.SplitList(env) {
		words := strings.Fields(entry)
		if len(words) == 0 {
			continue
		}
		placed := false
		for i, w := range words {
			if strings.Contains(w, "%s") {
				words[i], placed = strings.ReplaceAll(w, "%s", u), true
			}
		}
		if !placed {
			words = append(words, u)
		}
		out = append(out, words)
	}
	return out
}

// canOpenBrowser says whether a browser on this machine can reach a loopback
// listener here; over SSH the browser is on another machine. A BROWSER the
// person set counts as one.
var canOpenBrowser = func() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	if strings.TrimSpace(os.Getenv("BROWSER")) != "" {
		return true
	}
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return false
	}
	_, err := exec.LookPath("xdg-open")
	return err == nil
}

// signInOpener shows the person the browser sign-in address, and opens it
// unless they asked only to see it (-no-browser).
func signInOpener(noBrowser bool, errOut io.Writer, open func(string) error) func(string) error {
	return func(u string) error {
		if noBrowser {
			fmt.Fprintf(errOut, "Open this address in a browser on this machine to approve the sign-in:\n  %s\n", u)
			return nil
		}
		fmt.Fprintf(errOut, "Opening your browser to approve this sign-in. If it does not open, visit:\n  %s\n", u)
		if err := open(u); err != nil {
			fmt.Fprintf(errOut, "abhed: could not open a browser (%v); use the link above, or run abhed login -device\n", err)
		}
		return nil
	}
}

// maxDisplay bounds the name a server gives for the account, as kept and shown.
const maxDisplay = 80

// clipDisplay is a server-given name cut to maxDisplay characters.
func clipDisplay(s string) string {
	if r := []rune(s); len(r) > maxDisplay {
		return string(r[:maxDisplay-1]) + "…"
	}
	return s
}

// signedInAs is who a profile is signed in as, as a person reads it: the
// account's email, else its name, else the subject the server knows it by.
func signedInAs(p *profiles.Profile) string {
	if p.Display != "" {
		return p.Display
	}
	return p.Username
}

// profilesPath is where profiles are kept, refused inside the workspace,
// where the agent's commands could read the tokens.
func profilesPath(workspace string) (string, error) {
	p, err := profiles.DefaultPath()
	if err != nil {
		return "", err
	}
	under := func(dir string) bool {
		rel, err := filepath.Rel(dir, p)
		return err == nil && filepath.IsLocal(rel)
	}
	// A .abhed, the workspace's or the home folder's, is denied to commands whole.
	home, _ := os.UserHomeDir()
	shielded := under(filepath.Join(workspace, ".abhed")) || (home != "" && under(filepath.Join(home, ".abhed")))
	if workspace != "" && under(workspace) && !shielded {
		return "", fmt.Errorf("the profiles file %s is inside the workspace, where commands could read its tokens; set %s outside it", p, profiles.EnvFile)
	}
	return p, nil
}

// profileName is the default name for a server: its host and port.
func profileName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "default"
	}
	n := strings.NewReplacer(":", "-", "[", "", "]", "").Replace(u.Host)
	if !profiles.ValidName(n) {
		return "default"
	}
	return n
}

func loginCmd(workspace string, args []string, out, errOut io.Writer) int {
	return loginWith(workspace, args, out, errOut, loginOpts{open: openBrowser})
}

// loginOpts are what a caller of abhed login other than the command line
// chooses: what opens the browser sign-in page (the person's browser, or a
// request to a client such as Studio), and the profiles file to keep the
// sign-in in, in place of the person's.
type loginOpts struct {
	open func(string) error
	file string
	// ctx, when set, ends the sign-in early: a client's cancel.
	ctx context.Context
}

// loginWith is abhed login with o.
func loginWith(workspace string, args []string, out, errOut io.Writer, o loginOpts) int {
	if !profileNetwork(errOut) {
		return 1
	}
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(errOut)
	name := fs.String("profile", "", "profile name (default: the server's host and port)")
	device := fs.Bool("device", false, "sign in with a code entered in any browser (default over SSH)")
	browser := fs.Bool("browser", false, "sign in through a browser on this machine")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in address instead of opening a browser")
	method := fs.String("method", "", "the server's sign-in method to use, when it offers several")
	ca := fs.String("ca", "", "trust only this CA for the server (PEM)")
	cert := fs.String("cert", "", "client certificate, for a server that asks for one (PEM)")
	key := fs.String("key", "", "client certificate key (PEM)")
	allowHTTP := fs.Bool("allow-http", false, "allow plain http to a host other than this machine")
	var policyKeys policyKeyFlag
	fs.Var(&policyKeys, "policy-key", "pin this org policy signing key (base64 Ed25519 or PEM; repeatable) instead of the one the server offers")
	orgPolicy := fs.Bool("org-policy", false, "take the server's signed org policy without asking")
	fs.Usage = func() {
		fmt.Fprintln(errOut, "usage: abhed login [flags] URL\n  Signs this command line in to an Abhed server and saves it as a profile.\n  When the server serves a signed org policy, it asks before taking it (or -org-policy).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || (*device && *browser) {
		fs.Usage()
		return 2
	}
	fail := func(err error) int { fmt.Fprintf(errOut, "abhed: %v\n", err); return 1 }
	base, err := profiles.NormalizeURL(fs.Arg(0), *allowHTTP)
	if err != nil {
		return fail(err)
	}
	path := o.file
	if path == "" {
		if path, err = profilesPath(workspace); err != nil {
			return fail(err)
		}
	}
	reg, err := profiles.Load(path)
	if err != nil {
		return fail(err)
	}
	retryLogouts(path, reg, errOut)
	if *name == "" {
		*name = profileName(base)
		for n, p := range reg.Profiles {
			if p.URL == base {
				*name = n
				break
			}
		}
	}
	if !profiles.ValidName(*name) {
		return fail(fmt.Errorf("profile name %q: use letters, digits, '.', '_' or '-'", *name))
	}
	p := &profiles.Profile{URL: base}
	for _, f := range []struct {
		dst *string
		src string
	}{{&p.CAFile, *ca}, {&p.CertFile, *cert}, {&p.KeyFile, *key}} {
		if f.src != "" {
			abs, err := filepath.Abs(f.src)
			if err != nil {
				return fail(err)
			}
			*f.dst = abs
		}
	}
	client, err := p.HTTPClient()
	if err != nil {
		return fail(err)
	}
	ctx := o.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	d, err := profiles.Discover(ctx, client, base)
	if err != nil {
		return fail(err)
	}
	p.AuthMode = d.AuthMode
	switch {
	case d.AuthMode == "none" || d.AuthMode == "":
		fmt.Fprintf(out, "%s asks for no sign-in; saved it as profile %s.\n", base, *name)
		if code := saveProfile(path, reg, *name, p, errOut); code != 0 {
			return code
		}
		return enrollAfterLogin(ctx, path, *name, p, enrolOptions{given: policyKeys, accept: *orgPolicy || len(policyKeys) > 0, confirm: askYes}, out, errOut)
	case len(d.Methods) == 0:
		return fail(fmt.Errorf("%s (auth.mode %s) does not offer command-line sign-in; its operator can turn on auth.cli.enabled", base, d.AuthMode))
	}
	m := d.Methods[0]
	if *method != "" {
		found := false
		for _, cand := range d.Methods {
			if cand.Name == *method {
				m, found = cand, true
			}
		}
		if !found {
			return fail(fmt.Errorf("%s offers no sign-in method %q", base, *method))
		}
	}
	useDevice := *device || (!*browser && !canOpenBrowser())
	if useDevice && m.DeviceAuthorizationEndpoint == "" {
		return fail(fmt.Errorf("method %s offers no device-code sign-in; use -browser", m.Name))
	}
	if !useDevice && m.AuthorizationEndpoint == "" {
		return fail(fmt.Errorf("method %s offers no browser sign-in; use -device", m.Name))
	}
	var tok *profiles.Token
	if useDevice {
		host, _ := os.Hostname()
		tok, err = profiles.DeviceLogin(ctx, client, base, m, "abhed on "+host, func(code, uri, complete string) {
			fmt.Fprint(errOut, devicePrompt(base, uri, code))
			if complete != "" {
				fmt.Fprintf(errOut, "or open %s\n", complete)
			}
			fmt.Fprintln(errOut, "Waiting for approval…")
		})
	} else {
		tok, err = profiles.PKCELogin(ctx, client, base, m, signInOpener(*noBrowser, errOut, o.open))
	}
	if err != nil {
		return fail(profiles.GrantRemoved(base, err))
	}
	p.Method = m.Name
	p.ClientID = m.ClientID
	if p.TokenEndpoint, err = profiles.Resolve(base, m.TokenEndpoint); err != nil {
		return fail(err)
	}
	if m.RevocationEndpoint != "" {
		if p.RevocationEndpoint, err = profiles.Resolve(base, m.RevocationEndpoint); err != nil {
			return fail(err)
		}
	}
	p.Apply(tok, time.Now())
	me, err := profiles.Whoami(ctx, client, p)
	if err != nil {
		return fail(fmt.Errorf("signed in, but the token does not work: %w", err))
	}
	if sub, _ := me["subject"].(string); sub != "" {
		p.Username = sub
	}
	// The subject stays the account's key; a person reads the email or name.
	p.Display = ""
	for _, k := range []string{"email", "name"} {
		if v, _ := me[k].(string); strings.TrimSpace(v) != "" && v != p.Username {
			p.Display = clipDisplay(config.PrintableText(v))
			break
		}
	}
	if code := saveProfile(path, reg, *name, p, errOut); code != 0 {
		return code
	}
	until := ""
	if !p.ExpiresAt.IsZero() {
		until = ", until " + p.ExpiresAt.Local().Format("2 Jan 15:04")
	}
	fmt.Fprintf(out, "Signed in to %s as %s (profile %s%s).\n", base, signedInAs(p), *name, until)
	return enrollAfterLogin(ctx, path, *name, p, enrolOptions{given: policyKeys, accept: *orgPolicy || len(policyKeys) > 0, confirm: askYes}, out, errOut)
}

// enrollAfterLogin takes the server's signed org policy, if it serves one.
func enrollAfterLogin(ctx context.Context, path, name string, p *profiles.Profile, o enrolOptions, out, errOut io.Writer) int {
	if err := enrollOrgPolicy(ctx, path, name, p, o, out, errOut); err != nil {
		fmt.Fprintf(errOut, "abhed: org policy: %v\n", err)
		return 1
	}
	return 0
}

func saveProfile(path string, reg *profiles.File, name string, p *profiles.Profile, errOut io.Writer) int {
	reg.Profiles[name] = p
	reg.Current = name
	if err := profiles.Save(path, reg); err != nil {
		fmt.Fprintf(errOut, "abhed: %v\n", err)
		return 1
	}
	return 0
}

func logoutCmd(workspace string, args []string, out, errOut io.Writer) int {
	if !profileNetwork(errOut) {
		return 1
	}
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(errOut)
	name := fs.String("profile", "", "profile to sign out (default: the current one)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "usage: abhed logout [-profile NAME]")
		return 2
	}
	fail := func(err error) int { fmt.Fprintf(errOut, "abhed: %v\n", err); return 1 }
	path, err := profilesPath(workspace)
	if err != nil {
		return fail(err)
	}
	reg, err := profiles.Load(path)
	if err != nil {
		return fail(err)
	}
	retryLogouts(path, reg, errOut)
	n, p, err := reg.Get(*name)
	if err != nil {
		return fail(err)
	}
	if p.Token == "" {
		fmt.Fprintf(out, "Profile %s is not signed in.\n", n)
		return 0
	}
	// The token is not used here again either way. A sign-out the server did
	// not record is kept and sent again, so it is never quietly lost.
	client, rerr := p.HTTPClient()
	if rerr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		rerr = profiles.Logout(ctx, client, p, logoutAttempts, logoutBackoff)
		cancel()
	}
	reg.SignOut(n, rerr)
	if err := profiles.Save(path, reg); err != nil {
		return fail(err)
	}
	if rerr != nil {
		fmt.Fprintf(errOut, "abhed: %s could not record the sign-out (%v). The token is no longer used here, but it stays valid on the server; abhed will tell the server again on your next login, logout or profile command.\n", p.URL, rerr)
		return 1
	}
	fmt.Fprintf(out, "Signed out of %s (profile %s).\n", p.URL, n)
	return 0
}

// How abhed logout retries a server that could not record the sign-out.
var (
	logoutAttempts = 4
	logoutBackoff  = 500 * time.Millisecond
)

// retryLogouts tells servers again about sign-outs they did not record,
// saving the profiles file when one now has.
func retryLogouts(path string, reg *profiles.File, out io.Writer) {
	if len(reg.PendingLogouts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := reg.RetryLogouts(ctx, (*profiles.Profile).HTTPClient)
	if len(done) == 0 {
		return
	}
	if err := profiles.Save(path, reg); err != nil {
		fmt.Fprintf(out, "abhed: %v\n", err)
		return
	}
	for _, u := range done {
		fmt.Fprintf(out, "%s has now recorded the earlier sign-out.\n", u)
	}
}

func profileCmd(workspace string, args []string, out, errOut io.Writer) int {
	if !profileNetwork(errOut) {
		return 1
	}
	usage := func() int {
		fmt.Fprintln(errOut, "usage: abhed profile list | use NAME | rm NAME")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	fail := func(err error) int { fmt.Fprintf(errOut, "abhed: %v\n", err); return 1 }
	path, err := profilesPath(workspace)
	if err != nil {
		return fail(err)
	}
	reg, err := profiles.Load(path)
	if err != nil {
		return fail(err)
	}
	retryLogouts(path, reg, errOut)
	switch args[0] {
	case "list", "ls":
		if len(args) != 1 {
			return usage()
		}
		if len(reg.Profiles) == 0 {
			fmt.Fprintln(out, "no profiles; run abhed login URL")
			return 0
		}
		now := time.Now()
		for _, n := range reg.Names() {
			p := reg.Profiles[n]
			mark := " "
			if n == reg.Current {
				mark = "*"
			}
			status := "signed out"
			switch {
			case p.AuthMode == "none":
				status = "no sign-in needed"
			case p.PendingLogout:
				status = "signed out here; the server has not recorded it yet"
			case p.SignedIn(now) && p.ExpiresAt.IsZero():
				status = "signed in as " + signedInAs(p)
			case p.SignedIn(now):
				status = "signed in as " + signedInAs(p) + " until " + p.ExpiresAt.Local().Format("2 Jan 15:04")
			case p.Token != "":
				status = "expired; run abhed login " + p.URL
			}
			fmt.Fprintf(out, "%s %-20s %-36s %s\n", mark, n, p.URL, status)
		}
		return 0
	case "use":
		if len(args) != 2 {
			return usage()
		}
		if _, ok := reg.Profiles[args[1]]; !ok {
			return fail(fmt.Errorf("no profile %q; abhed profile list shows them", args[1]))
		}
		reg.Current = args[1]
		if err := profiles.Save(path, reg); err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Using profile %s (%s).\n", args[1], reg.Profiles[args[1]].URL)
		return 0
	case "rm", "remove":
		if len(args) != 2 {
			return usage()
		}
		p, ok := reg.Profiles[args[1]]
		if !ok {
			return fail(fmt.Errorf("no profile %q", args[1]))
		}
		if p.Token != "" {
			if client, err := p.HTTPClient(); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_ = profiles.Revoke(ctx, client, p)
				cancel()
			}
		}
		delete(reg.Profiles, args[1])
		if reg.Current == args[1] {
			reg.Current = ""
		}
		if err := profiles.Save(path, reg); err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Removed profile %s.\n", args[1])
		return 0
	}
	return usage()
}

// devicePrompt tells the person where to enter a device code. Single sign-on
// hands out the identity provider's page, where one signs in to the provider.
func devicePrompt(base, uri, code string) string {
	where := "in a browser where you are signed in to the server"
	if u, err := url.Parse(uri); err == nil && u.Host != "" && !strings.EqualFold(u.Host, hostOf(base)) {
		where = "in a browser and sign in there"
	}
	return fmt.Sprintf("To sign in, open %s %s, and enter the code\n\n    %s\n\n", uri, where, code)
}

// hostOf is a URL's host:port, or "" when it does not parse.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// askYes asks on the terminal and reports a yes; with no terminal it is no.
var askYes = func(prompt string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return false
	}
	fmt.Fprint(os.Stderr, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}
