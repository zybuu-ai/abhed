//go:build unix

package hostgit

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// prePushRelay runs the person's pre-push hook in the sandbox for a push
// host git makes: host git's hooks folder is one of Abhed's own, whose only
// hook hands its arguments and input (the refs to push) back here through
// a pipe and waits for the answer; Abhed runs the person's hook in the
// sandbox with them and gives back its output and exit. No hook runs on the
// host, and one that chains into the work tree gains nothing the sandbox
// does not already have: no key, no agent socket, no credential helper.
type prePushRelay struct {
	dir  string
	req  *os.File
	mu   sync.Mutex
	ran  bool
	code int
	done chan struct{}
	once sync.Once
}

// relayEnd ends what the relay's hook sends: never a line of git's input,
// which is four words of refs and object names.
const relayEnd = "--abhed-pre-push-end--"

// relayHook is the hook host git runs: it sends its two arguments and its
// input, waits for the exit, prints the output and exits with it.
const relayHook = `#!/bin/sh
# Abhed's relay: the person's pre-push hook runs in the sandbox, not here.
d=$(dirname "$0")
{ printf '%s\n' "$1" "$2"; cat; printf '%s\n' '` + relayEnd + `'; } > "$d/req"
read ready < "$d/resp"
cat "$d/out" >&2
read code < "$d/code"
exit "${code:-1}"
`

func (b *HostBroker) prePushRelay(ctx context.Context, p plan) (*prePushRelay, error) {
	parent, err := relayParent()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(parent, "pre-push-*")
	if err != nil {
		return nil, err
	}
	r := &prePushRelay{dir: dir, done: make(chan struct{})}
	fail := func(err error) (*prePushRelay, error) {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	for _, f := range []string{"req", "resp"} {
		if err := unix.Mkfifo(filepath.Join(dir, f), 0o600); err != nil {
			return fail(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pre-push"), []byte(relayHook), 0o700); err != nil { // #nosec G306 -- a hook git runs, private folder
		return fail(err)
	}
	// Opened for reading and writing, so the open waits for no one.
	if r.req, err = os.OpenFile(filepath.Join(dir, "req"), os.O_RDWR, 0); err != nil {
		return fail(err)
	}
	go r.serve(ctx, b, p)
	return r, nil
}

// relayParent is where relays are made; tests set it.
var relayParent = func() (string, error) { return privateDir("pre-push") }

// serve answers the relay's hook once: the person's pre-push, run in the
// sandbox at the repository's top with git's arguments and input.
func (r *prePushRelay) serve(ctx context.Context, b *HostBroker, p plan) {
	defer close(r.done)
	sc := bufio.NewScanner(r.req)
	var lines []string
	for sc.Scan() {
		if sc.Text() == relayEnd {
			break
		}
		lines = append(lines, sc.Text())
	}
	if len(lines) < 2 || sc.Err() != nil {
		return // closed: the push ended without asking
	}
	remote, url, input := lines[0], redact(lines[1]), lines[2:]
	hook := filepath.Join(p.hooks, "pre-push")
	// No check that the sandbox can see it: one it cannot run fails, and the push with it.
	line := "exec " + shellQuote(hook) + " " + shellQuote(remote) + " " + shellQuote(url) +
		" <<'ABHED_PRE_PUSH'\n" + strings.Join(input, "\n") + "\nABHED_PRE_PUSH\n"
	cmd := b.command(ctx, p.root, line)
	var out bytes.Buffer
	code := 0
	switch {
	case cmd.Err != nil:
		code = 1
		fmt.Fprintf(&out, "abhed: your pre-push hook could not run in the sandbox: %v\n", cmd.Err)
		sandbox.Release(cmd)
	default:
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			sandbox.Release(cmd)
			code = 1
			fmt.Fprintf(&out, "abhed: your pre-push hook could not run in the sandbox: %v\n", err)
			break
		}
		err := cmd.Wait()
		sandbox.Exited(cmd, err)
		if err != nil {
			code = exitCode(err)
		}
	}
	r.mu.Lock()
	r.ran, r.code = true, code
	r.mu.Unlock()
	if os.WriteFile(filepath.Join(r.dir, "out"), []byte(redact(out.String())), 0o600) != nil ||
		os.WriteFile(filepath.Join(r.dir, "code"), []byte(strconv.Itoa(code)+"\n"), 0o600) != nil {
		return // the hook reads no exit and fails, so the push does not go on
	}
	// The hook waits to read resp; a writer that finds no reader yet tries again.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && ctx.Err() == nil; time.Sleep(20 * time.Millisecond) {
		fd, err := unix.Open(filepath.Join(r.dir, "resp"), unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		_, _ = unix.Write(fd, []byte("go\n"))
		_ = unix.Close(fd)
		return
	}
}

// close ends the relay, removing its folder, and says whether the hook ran
// and its exit.
func (r *prePushRelay) close() (ran bool, code int) {
	r.once.Do(func() {
		_ = r.req.Close()
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
		}
		_ = os.RemoveAll(r.dir)
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ran, r.code
}
