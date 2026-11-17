package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
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
	h, err := remote.NewHost(remote.HostConfig{Name: *host, Addr: *host + ":" + strconv.Itoa(*port), User: *user,
		IdentityFile: *identity, KnownHostsFile: *knownHosts})
	if err != nil {
		return fail("%v", err)
	}
	defer func() { _ = h.Close() }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	path := *engine
	if path == "" {
		path = `"$HOME/.abhed/bin/abhed-` + want[:16] + `"`
	} else {
		path = shellQuote(path)
	}
	if local != nil {
		// Copied beside its final name and checked there before it is put in place.
		put := sshEngineCheck(path, want) + ` && exit 0; ` +
			`umask 077; mkdir -p "$(dirname ` + path + `)" && t=` + path + `.part && cat > "$t" && ` +
			`[ "$(sum "$t")" = ` + want + ` ] && chmod 500 "$t" && mv -f "$t" ` + path
		out, err := h.RunInput(ctx, "sh -c "+shellQuote(sshSumFunc+put), 0, local)
		if err != nil {
			return fail("%v", err)
		}
		if out.ExitCode != 0 {
			return fail("the engine could not be copied to %s: %s", *host, strings.TrimSpace(out.Stderr))
		}
	}
	// Checked at every start, then run in place of the shell.
	script := sshSumFunc + sshEngineCheck(path, want) + ` || { echo "abhed: the engine on this host (` + strings.ReplaceAll(path, `"`, ``) + `) is missing or does not have the pinned SHA-256 ` + want + `; refused" >&2; exit 97; }; ` +
		`cd ` + shellQuote(folder) + ` || exit 98; exec ` + path + ` acp`
	code, err := h.Stream(ctx, "sh -c "+shellQuote(script), stdin, stdout, stderr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return fail("%v", err)
	}
	return code
}

var (
	hostRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,252})$|^\[?[0-9a-fA-F:.]+\]?$`)
	userRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
)

// sshSumFunc defines sum FILE: its SHA-256, with whichever tool the host has.
const sshSumFunc = `sum() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d" " -f1; }; `

// sshEngineCheck is a shell test that path is a regular file with the pinned hash.
func sshEngineCheck(path, want string) string {
	return `{ [ -f ` + path + ` ] && [ ! -L ` + path + ` ] && [ "$(sum ` + path + `)" = ` + want + ` ]; }`
}
