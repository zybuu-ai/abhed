package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/zybuu-ai/abhed/internal/remote"
)

// sshACPCmd is `abhed ssh-acp`: an editor's engine on an SSH host
// (docs/architecture/studio-remote.md). It connects with the engine's SSH
// client (strict known_hosts, never an insecure skip), checks the engine
// binary on the host against the SHA-256 the person pinned, uploading it
// first when asked, and runs `abhed acp` there in the folder, carrying ACP over
// its own stdio. Nothing is forwarded: no agent, no X11, no ports, and no
// environment is sent. The engine on the host keeps its own policy, sandbox,
// configuration and record.
func sshACPCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ssh-acp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "the SSH host (name or address)")
	port := fs.Int("port", 22, "its port")
	user := fs.String("user", "", "the account on it")
	identity := fs.String("identity", "", "a private key file; empty uses your SSH agent to sign in (never forwarded)")
	knownHosts := fs.String("known-hosts", "", "the known_hosts file the host key must be in (default ~/.ssh/known_hosts)")
	engine := fs.String("engine", "", "the engine's path on the host; with -upload, empty is ~/.abhed/bin/abhed-<sha256>")
	pin := fs.String("sha256", "", "the engine's SHA-256; the host's binary must have it")
	upload := fs.String("upload", "", "a local engine build for the host, with that SHA-256, copied there when the host lacks it")
	usage := func() int {
		fmt.Fprintln(stderr, "usage: abhed ssh-acp -host H -user U [-port N] [-identity F] [-known-hosts F] -sha256 HEX (-engine PATH | -upload FILE) -- FOLDER\n"+
			"  Runs `abhed acp` on the host in FOLDER, for an editor; ACP is carried on this command's stdio.")
		return 2
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		return usage()
	}
	fail := func(format string, a ...any) int { fmt.Fprintf(stderr, "abhed: "+format+"\n", a...); return 1 }
	if why := inAgentCommand(); why != "" {
		return fail("ssh-acp signs in to a host as you and is refused inside an agent's command (%s)", why)
	}
	folder := fs.Arg(0)
	want := strings.ToLower(*pin)
	switch {
	case !hostRE.MatchString(*host) || !userRE.MatchString(*user) || *port < 1 || *port > 65535:
		return fail("-host, -user and -port name an SSH account")
	case !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(want):
		return fail("-sha256 is the engine's SHA-256 in hex")
	case !strings.HasPrefix(folder, "/") || strings.ContainsAny(folder, "\x00\n"):
		return fail("the folder is an absolute path on the host")
	case *engine == "" && *upload == "":
		return fail("give -engine (the pinned binary on the host) or -upload (a local build to copy there)")
	case *engine != "" && (!strings.HasPrefix(*engine, "/") || strings.ContainsAny(*engine, "\x00\n")):
		return fail("-engine is an absolute path on the host")
	}
	var local []byte
	if *upload != "" {
		data, err := os.ReadFile(*upload) // #nosec G304 -- the person's own engine build, named by them
		if err != nil {
			return fail("read %s: %v", *upload, err)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != want {
			return fail("%s does not have the pinned SHA-256; nothing was sent", *upload)
		}
		local = data
	}
	bare := strings.TrimSuffix(strings.TrimPrefix(*host, "["), "]")
	h, err := remote.NewHost(remote.HostConfig{Name: bare, Addr: net.JoinHostPort(bare, strconv.Itoa(*port)), User: *user,
		IdentityFile: *identity, KnownHostsFile: *knownHosts})
	if err != nil {
		return fail("%v", err)
	}
	defer func() { _ = h.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if local != nil {
		// The build arrives on stdin, into the account's private engine folder.
		out, err := h.RunInput(ctx, "sh -c "+shellQuote(sshPutScript(want)), 0, local)
		if err != nil {
			return fail("%v", err)
		}
		if out.ExitCode != 0 {
			return fail("the engine could not be copied to %s: %s", *host, strings.TrimSpace(out.Stderr))
		}
	}
	code, err := h.Stream(ctx, "sh -c "+shellQuote(sshStartScript(*engine, want, folder)), stdin, stdout, stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return fail("%v", err)
	}
	return code
}

// sshPrelude is the shell both scripts start with: strict, private files, a
// refusal that prints its words as data, sum FILE (its SHA-256, with whichever
// tool the host has), and safe PATH: not a link, owned by the account or
// root, writable by neither group nor others.
const sshPrelude = `set -eu; umask 077; ` +
	`fail() { printf 'abhed: %s\n' "$*" >&2; exit 97; }; ` +
	`sum() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d" " -f1; }; ` +
	`me=$(id -u); ` +
	`safe() { [ ! -L "$1" ] && [ -n "$(find "$1" -maxdepth 0 \( -user "$me" -o -user 0 \) ! -perm -0020 ! -perm -0002 -print 2>/dev/null)" ]; }; ` +
	// The engine lives in ~/.abhed/bin, made private here; the folders above it must be the account's own.
	`[ -n "${HOME:-}" ] && safe "$HOME" || fail "your home folder is writable by others or is not yours, so an engine kept there could be swapped"; ` +
	`mkdir -p "$HOME/.abhed/bin"; chmod 700 "$HOME/.abhed" "$HOME/.abhed/bin"; ` +
	`safe "$HOME/.abhed" && safe "$HOME/.abhed/bin" || fail "~/.abhed or ~/.abhed/bin is a link, not yours, or writable by others"; `

// sshEngineFile is where the verified engine is run from: a private folder of the account's.
func sshEngineFile(want string) string { return `"$HOME/.abhed/bin/abhed-` + want[:16] + `"` }

// sshPlace copies from (a shell word, or empty for stdin) to the engine's
// place beside it, checks the copy's hash, and puts it there; a link where the
// copy goes is refused.
func sshPlace(from, want string) string {
	src := ""
	if from != "" {
		src = " < " + from
	}
	return `e=` + sshEngineFile(want) + `; t="$e.part"; ` +
		`[ ! -L "$t" ] || fail "$t is a link"; rm -f "$t"; ` +
		`(set -C; cat` + src + ` > "$t") || fail "the engine could not be copied"; ` +
		`chmod 500 "$t"; ` +
		`[ "$(sum "$t")" = ` + want + ` ] || { rm -f "$t"; fail "the engine does not have the pinned SHA-256 ` + want + `; refused"; }; ` +
		`mv -f "$t" "$e"; `
}

// sshPutScript receives an uploaded build on stdin into the private folder,
// unless a verified copy is there already.
func sshPutScript(want string) string {
	return sshPrelude + `e=` + sshEngineFile(want) + `; ` +
		`if [ -f "$e" ] && safe "$e" && [ "$(sum "$e")" = ` + want + ` ]; then cat >/dev/null; exit 0; fi; ` +
		sshPlace("", want)
}

// sshStartScript is the one remote step that runs the engine: the pinned
// binary (from enginePath, when given) is copied into the private folder if a
// verified copy is not there, the copy's place, owner, mode and hash are
// checked, and that path is exec'd, so what is checked is what runs and no
// other account can swap it in between.
func sshStartScript(enginePath, want, folder string) string {
	s := sshPrelude + `e=` + sshEngineFile(want) + `; `
	if enginePath != "" {
		s += `if ! { [ -f "$e" ] && safe "$e" && [ "$(sum "$e")" = ` + want + ` ]; }; then ` +
			`[ -f ` + shellQuote(enginePath) + ` ] || fail "there is no engine at" ` + shellQuote(enginePath) + `; ` +
			sshPlace(shellQuote(enginePath), want) + `fi; `
	}
	return s + `[ -f "$e" ] && safe "$e" && [ "$(sum "$e")" = ` + want + ` ] || fail "the engine on this host is missing or does not have the pinned SHA-256 ` + want + `; refused"; ` +
		`cd ` + shellQuote(folder) + ` || exit 98; exec "$e" acp`
}

var (
	hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})$|^\[?[0-9a-fA-F:.]+\]?$`)
	userRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
)
