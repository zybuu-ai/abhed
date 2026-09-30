package app

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/zybuu-ai/abhed/internal/secrets"
	"golang.org/x/term"
)

// openVault opens the secrets store. A missing file is an empty store, so a
// deployment with no secrets pays nothing and needs no configuration.
func openVault() *secrets.Store { return secrets.Default() }

// vaultLoads refuses a session whose secrets store exists but cannot be loaded,
// since its values could not be redacted.
func vaultLoads() error {
	_, err := openVault().LoadRedactor()
	return err
}

// vaultNames lists what the model may ask for. An unreadable store lists
// nothing: the failure surfaces when a secret is used, with its reason.
func vaultNames(v *secrets.Store) []string {
	names, err := v.Names()
	if err != nil {
		return nil
	}
	return names
}

// secretCmd manages the store: set NAME (value on stdin or prompted), list, rm NAME.
func secretCmd(args []string) int {
	vault := openVault()
	fail := func(err error) int { fmt.Fprintf(os.Stderr, "abhed: %v\n", err); return 1 }
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: abhed secret set NAME | list | rm NAME\n"+
			"  The value is read from stdin, or prompted without echo on a terminal.\n"+
			"  Stored in "+vault.Path()+" ("+secrets.EnvFile+" overrides), mode 600, never in config.\n"+
			"  A session may use a secret only under an allow rule: \"allow\": [\"secret(NAME)\"].")
		return 2
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "list":
		names, err := vault.Names()
		if err != nil {
			return fail(err)
		}
		if len(names) == 0 {
			fmt.Println("no secrets stored in " + vault.Path())
			return 0
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return 0
	case "rm", "remove":
		if len(args) != 2 {
			return usage()
		}
		if err := vault.Remove(args[1]); err != nil {
			return fail(err)
		}
		fmt.Printf("removed %s\n", args[1])
		return 0
	case "set":
		if len(args) != 2 {
			return usage()
		}
		var value string
		if term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Fprintf(os.Stderr, "value for %s (not echoed): ", args[1])
			b, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return fail(err)
			}
			value = string(b)
		} else {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fail(err)
			}
			value = strings.TrimRight(string(b), "\r\n")
		}
		// A short value would also match ordinary text and be redacted there.
		if n := utf8.RuneCountInString(value); n > 0 && n < secrets.MinLength {
			return fail(fmt.Errorf("the value is %d characters; a secret must be at least %d, or redaction would match ordinary text", n, secrets.MinLength))
		}
		if err := vault.Set(args[1], value); err != nil {
			return fail(err)
		}
		fmt.Printf("stored %s in %s\n", args[1], vault.Path())
		return 0
	}
	return usage()
}
