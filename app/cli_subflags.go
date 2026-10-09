package app

import (
	"flag"
	"fmt"
	"strings"

	"github.com/zybuu-ai/abhed/config"
)

// The command line's own flags written after a built-in subcommand. They
// used to be left to the subcommand, and one that reads no flags ignored
// them: `abhed doctor -C ws` checked the current folder instead.

// subOwnFlags are flags a subcommand declares itself under the name of one
// of the command line's; written after the subcommand they are its own.
// A new subcommand flag that shares a top-level name belongs here.
var subOwnFlags = map[string][]string{
	"serve":    {"addr", "trust-workspace"},
	"eval":     {"trust-workspace"},
	"resolve":  {"mode", "allow", "trust-workspace"},
	"record":   {"n"},
	"sessions": {"n"},
	"user":     {"name"},
	"pki":      {"name"},
	"sandbox":  {"name", "c"},
}

// noWorkspace are the subcommands that never read the workspace, so -C
// means nothing to them.
var noWorkspace = map[string]bool{"providers": true, "secret": true, "pki": true, "admin": true}

// noArgs are the subcommands that take no arguments; doctor and version take
// only --json.
var noArgs = map[string]bool{"init": true, "providers": true, "acp": true, "rpc": true, "index": true,
	"doctor": true, "version": true}

// verbatimFrom is where a subcommand's arguments start being passed on as
// given, counted in words that are not flags (its verb included): mcp add's
// server command and sandbox exec's and ssh's command.
var verbatimFrom = map[string]int{"mcp": 3, "sandbox": 3}

// takesGlobal reports whether the subcommand reads the top-level flag.
func takesGlobal(sub, name string) bool {
	switch name {
	case "C":
		return !noWorkspace[sub]
	case "trust-workspace":
		for _, c := range subcommands {
			if c.name == sub {
				return c.trust
			}
		}
	case "settings":
		return sub == "doctor"
	}
	return false
}

// hoistGlobalFlags sets on fs the top-level flags written after the
// subcommand in sub (sub[0] is its name) that the subcommand reads, and
// returns the arguments left for it. A top-level flag it does not read, or
// an argument to a subcommand that takes none, is an error: exit code 2.
func hoistGlobalFlags(fs *flag.FlagSet, sub []string) ([]string, error) {
	name := sub[0]
	own := map[string]bool{}
	for _, n := range subOwnFlags[name] {
		own[n] = true
	}
	left := []string{name}
	words := 1
	for i := 1; i < len(sub); i++ {
		arg := sub[i]
		flagName, value, hasValue, isFlag := splitFlag(arg)
		g := fs.Lookup(flagName)
		global := isFlag && g != nil && !own[flagName]
		if !global {
			if err := checkArg(name, arg); err != nil {
				return nil, err
			}
		}
		if arg == "--" || (verbatimFrom[name] > 0 && words >= verbatimFrom[name]) {
			left = append(left, sub[i:]...)
			break
		}
		if !global {
			if !isFlag {
				words++
			}
			left = append(left, arg)
			continue
		}
		if !takesGlobal(name, flagName) {
			return nil, fmt.Errorf("-%s does not apply to %s; see abhed %s -h", flagName, name, name)
		}
		if !hasValue {
			if b, ok := g.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
				value = "true"
			} else if i+1 < len(sub) {
				i++
				value = sub[i]
			} else {
				return nil, fmt.Errorf("-%s needs a value", flagName)
			}
		}
		if err := fs.Set(flagName, value); err != nil {
			return nil, fmt.Errorf("-%s: %w", flagName, err)
		}
	}
	return left, nil
}

// checkArg refuses an argument, other than a top-level flag, to a
// subcommand that takes none.
func checkArg(name, arg string) error {
	if !noArgs[name] || isHelpArg(arg) {
		return nil
	}
	if name == "doctor" || name == "version" {
		if arg == "--json" || arg == "-json" {
			return nil
		}
		return fmt.Errorf("%s takes only --json; got %s", name, config.Printable(arg))
	}
	return fmt.Errorf("%s takes no arguments; got %s", name, config.Printable(arg))
}

// splitFlag reads "-name", "--name" or "-name=value" the way the flag
// package does; "-" alone and anything else is not a flag.
func splitFlag(arg string) (name, value string, hasValue, ok bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", "", false, false
	}
	name = strings.TrimPrefix(arg[1:], "-")
	if name == "" || name[0] == '-' || name[0] == '=' {
		return "", "", false, false
	}
	name, value, hasValue = strings.Cut(name, "=")
	return name, value, hasValue, true
}
