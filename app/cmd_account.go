package app

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/zybuu-ai/abhed/internal/identity"
)

// prompter asks a person for a line or a secret: from the terminal, with the
// secret unechoed, or one line at a time from a pipe for scripts and tests.
type prompter struct {
	in  *bufio.Reader
	fd  int
	tty bool
	out io.Writer
}

func newPrompter(in *os.File, out io.Writer) *prompter {
	return &prompter{in: bufio.NewReader(in), fd: int(in.Fd()), tty: term.IsTerminal(int(in.Fd())), out: out}
}

// errNoInput is input that ended before an answer.
var errNoInput = errors.New("no answer: the input ended")

func (p *prompter) line(q, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", q, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", q)
	}
	s, err := p.in.ReadString('\n')
	if err != nil && (s == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", errNoInput
		}
		return "", err
	}
	if s = strings.TrimSpace(s); s == "" {
		return def, nil
	}
	return s, nil
}

func (p *prompter) secret(q string) (string, error) {
	fmt.Fprintf(p.out, "%s: ", q)
	if p.tty {
		b, err := term.ReadPassword(p.fd)
		fmt.Fprintln(p.out)
		return string(b), err
	}
	s, err := p.in.ReadString('\n')
	if err != nil && s == "" {
		return "", errNoInput
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// newPassword asks for a password twice until the two agree and it is good.
func (p *prompter) newPassword(q string) (string, error) {
	for range 3 {
		pw, err := p.secret(q)
		if err != nil {
			return "", err
		}
		if err := identity.CheckPassword(pw); err != nil {
			fmt.Fprintf(p.out, "  %v\n", err)
			continue
		}
		again, err := p.secret("Type it again")
		if err != nil {
			return "", err
		}
		if again != pw {
			fmt.Fprintln(p.out, "  The two did not match.")
			continue
		}
		return pw, nil
	}
	return "", errors.New("no password set")
}

// defaultUsername is $USER as an account name, or "".
func defaultUsername() string {
	u := strings.ToLower(os.Getenv("USER"))
	if identity.CheckUsername(u) != nil {
		return ""
	}
	return u
}

func osUserAndHost() (string, string) {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return name, host
}

// resetNote is what a sign-in says about a reset since the last one.
func resetNote(r *identity.Reset) string {
	if r == nil {
		return ""
	}
	how := "with the recovery code"
	if r.By == "os-user" {
		how = "without the recovery code, by the OS user " + r.OSUser
		if r.Host != "" {
			how += " on " + r.Host
		}
	}
	return fmt.Sprintf("Your password was reset on %s %s. If that was not you, change it now: abhed account passwd.",
		r.At.Local().Format("2 Jan 2006 15:04"), how)
}

// accountCmd is `abhed account`: the accounts on this machine.
func accountCmd(args []string, stdin *os.File, out, errOut io.Writer) int {
	usage := func() {
		fmt.Fprintln(errOut, `usage: abhed account <command>
  create [-email E] [-name N] [USERNAME]   make an account on this machine and sign in
  signin [USERNAME]                         sign in; this machine remembers it until signout
  signout                                   forget the sign-in; the next start asks again
  whoami                                    who is signed in on this machine
  list                                      the accounts and server sign-ins on this machine
  passwd [USERNAME]                         change the password
  reset USERNAME [-as-os-user]              set a new password with the recovery code, or as the OS user
  signout-everywhere [USERNAME]             end every remembered sign-in of the account
  rm USERNAME [-delete-data]                remove the account; its data is kept unless -delete-data
  adopt [USERNAME]                          move the data from before sign-in into the account
  unmigrate                                 move it back to ~/.abhed, before going back to 1.2.x

Accounts are a separate space for each person using this OS login. They are not
encryption: anyone who can act as this OS user can read the files. Passwords
and answers are read from the terminal, or one per line from stdin.`)
	}
	if len(args) == 0 || isHelpArg(args[0]) {
		usage()
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	fail := func(err error) int { fmt.Fprintf(errOut, "abhed: %v\n", err); return 1 }
	s, err := identity.Open()
	if err != nil {
		return fail(err)
	}
	p := newPrompter(stdin, errOut)
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("account "+verb, flag.ContinueOnError)
	fs.SetOutput(errOut)
	email := fs.String("email", "", "the account's email, for display only")
	name := fs.String("name", "", "the account's name, for display only")
	asOS := fs.Bool("as-os-user", false, "reset without the recovery code, as the OS user; the next sign-in says so")
	deleteData := fs.Bool("delete-data", false, "also delete the account's folder: its sessions, history and memory")
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	arg := fs.Arg(0)
	switch verb {
	case "create":
		e, code, err := createAccount(s, p, arg, *email, *name)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Created %s and signed in. This machine remembers it until abhed account signout.\n", e.Display())
		fmt.Fprintf(out, "Recovery code (shown once; keep it somewhere safe): %s\n", code)
		return 0
	case "signin":
		e, err := signInAccount(s, p, arg)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Signed in as %s.\n", e.Display())
		return 0
	case "signout":
		if err := s.Forget(); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Signed out. The next start asks who is working.")
		return 0
	case "whoami":
		e, ok, err := s.Remembered(nil)
		if err != nil {
			return fail(err)
		}
		if !ok {
			fmt.Fprintln(out, "No one is signed in on this machine.")
			return 1
		}
		fmt.Fprintln(out, e.Display())
		return 0
	case "list":
		list, err := s.List()
		if err != nil {
			return fail(err)
		}
		rem, _, _ := s.Remembered(nil)
		if len(list) == 0 {
			fmt.Fprintln(out, "No accounts on this machine. abhed account create makes one.")
			return 0
		}
		for _, e := range list {
			mark := " "
			if e.ID == rem.ID {
				mark = "*"
			}
			fmt.Fprintf(out, "%s %-8s %s\n", mark, e.Kind, e.Display())
		}
		return 0
	case "passwd":
		u, err := accountName(s, p, arg)
		if err != nil {
			return fail(err)
		}
		cur, err := p.secret("Current password")
		if err != nil {
			return fail(err)
		}
		next, err := p.newPassword("New password")
		if err != nil {
			return fail(err)
		}
		if err := s.ChangePassword(u, cur, next); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Password changed. Every remembered sign-in of the account has ended; sign in again.")
		return 0
	case "reset":
		if arg == "" {
			usage()
			return 2
		}
		if *asOS {
			fmt.Fprintf(errOut, "This sets a new password for %s without the recovery code. Anyone who is this OS user\ncan do this; the account's next sign-in will say it happened.\n", arg)
			if ok, err := p.line("Type the username to confirm", ""); err != nil || ok != arg {
				return fail(errors.New("not confirmed; nothing changed"))
			}
			next, err := p.newPassword("New password")
			if err != nil {
				return fail(err)
			}
			osUser, host := osUserAndHost()
			if err := s.ResetAsOSUser(arg, next, osUser, host); err != nil {
				return fail(err)
			}
			fmt.Fprintln(out, "Password reset.")
			return 0
		}
		code, err := p.secret("Recovery code")
		if err != nil {
			return fail(err)
		}
		next, err := p.newPassword("New password")
		if err != nil {
			return fail(err)
		}
		fresh, err := s.ResetWithCode(arg, code, next)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Password reset. The old recovery code no longer works; the new one (shown once): %s\n", fresh)
		return 0
	case "signout-everywhere":
		u, err := accountName(s, p, arg)
		if err != nil {
			return fail(err)
		}
		e, err := s.Get(u)
		if err != nil {
			return fail(err)
		}
		if err := s.SignOutEverywhere(e.ID); err != nil {
			return fail(err)
		}
		fmt.Fprintln(out, "Every remembered sign-in of the account has ended.")
		return 0
	case "adopt":
		u, err := accountName(s, p, arg)
		if err != nil {
			return fail(err)
		}
		e, err := s.Get(u)
		if err != nil {
			return fail(err)
		}
		if !identity.LegacyData() {
			fmt.Fprintln(out, "There is no data from before sign-in in ~/.abhed to move.")
			return 0
		}
		res, err := identity.Migrate(s, e, legacyCLIUser())
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "Moved %d item(s) into %s; %d session record(s) verified before and after.\n", res.Moved, e.Display(), res.Sessions)
		return 0
	case "unmigrate":
		n, err := identity.Unmigrate(s)
		if err != nil {
			return fail(err)
		}
		_ = s.Forget()
		fmt.Fprintf(out, "Moved %d item(s) back to ~/.abhed. Earlier versions see them again; sessions recorded since sign-in stay this account's.\n", n)
		return 0
	case "rm", "remove":
		if arg == "" {
			usage()
			return 2
		}
		e, err := s.Get(arg)
		if err != nil {
			return fail(err)
		}
		if ok, err := p.line("Type the username to remove the account", ""); err != nil || ok != arg {
			return fail(errors.New("not confirmed; nothing changed"))
		}
		if rem, ok, _ := s.Remembered(nil); ok && rem.ID == e.ID {
			_ = s.Forget()
		}
		if err := s.Remove(e.ID); err != nil {
			return fail(err)
		}
		dir := filepath.Join(s.Dir(), e.ID)
		if *deleteData {
			if err := os.RemoveAll(dir); err != nil {
				return fail(err)
			}
			fmt.Fprintln(out, "Removed the account and its data.")
			return 0
		}
		fmt.Fprintf(out, "Removed the account. Its data is kept in %s.\n", dir)
		return 0
	}
	usage()
	return 2
}

// accountName is name, else the only account, else the remembered one, else asked.
func accountName(s *identity.Store, p *prompter, name string) (string, error) {
	if name != "" {
		return name, nil
	}
	if rem, ok, _ := s.Remembered(nil); ok && rem.Kind == identity.KindAccount {
		return rem.Username, nil
	}
	list, err := s.List()
	if err != nil {
		return "", err
	}
	var only string
	n := 0
	for _, e := range list {
		if e.Kind == identity.KindAccount {
			only, n = e.Username, n+1
		}
	}
	if n == 1 {
		return only, nil
	}
	return p.line("Username", "")
}

// createAccount asks for what an account needs, makes it, and signs it in.
func createAccount(s *identity.Store, p *prompter, username, email, name string) (identity.Entry, string, error) {
	var err error
	for username == "" || identity.CheckUsername(strings.ToLower(username)) != nil {
		if username != "" {
			fmt.Fprintf(p.out, "  %v\n", identity.ErrBadUsername)
		}
		if username, err = p.line("Username", defaultUsername()); err != nil {
			return identity.Entry{}, "", err
		}
		if username == "" {
			return identity.Entry{}, "", errNoInput
		}
	}
	pw, err := p.newPassword("Password (at least 10 characters)")
	if err != nil {
		return identity.Entry{}, "", err
	}
	e, code, err := s.Create(username, pw, email, name)
	if err != nil {
		return identity.Entry{}, "", err
	}
	if err := s.Remember(e.ID); err != nil {
		return identity.Entry{}, "", err
	}
	return e, code, nil
}

// signInAccount asks for the password, up to three times, and remembers the
// sign-in.
func signInAccount(s *identity.Store, p *prompter, username string) (identity.Entry, error) {
	username, err := accountName(s, p, username)
	if err != nil {
		return identity.Entry{}, err
	}
	var e identity.Entry
	for try := 0; ; try++ {
		pw, err := p.secret("Password for " + username)
		if err != nil {
			return identity.Entry{}, err
		}
		if e, err = s.Authenticate(username, pw); err == nil {
			break
		}
		var locked *identity.LockedError
		if try == 2 || errors.As(err, &locked) || !errors.Is(err, identity.ErrBadPassword) {
			return identity.Entry{}, err
		}
		fmt.Fprintln(p.out, "  Incorrect username or password.")
	}
	if err := s.Remember(e.ID); err != nil {
		return identity.Entry{}, err
	}
	if r, _ := s.TakeReset(e.ID); r != nil {
		fmt.Fprintln(p.out, resetNote(r))
	}
	return e, nil
}
