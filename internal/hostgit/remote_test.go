package hostgit

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zybuu-ai/abhed/internal/sandbox"
)

// remoteRig is a workspace repository whose origin is git@git.example:team/app.git,
// reached through an ssh stub in a throwaway HOME's global configuration that
// serves a local bare repository: no network. The stub logs what it was
// given; a private key and an agent socket name hold markers that must never
// reach a command's output or the record.
type remoteRig struct {
	ws, bare, home, sshLog string
	inner                  []string
	// runInner runs the sandbox's lines, with no credential and no network,
	// as a pull's merge or a clone's checkout needs.
	runInner bool
	// holdsNew is what the rig's sandbox says of holding repositories
	// cloned during the session against commands already running.
	holdsNew bool
	mu       sync.Mutex
	events   []map[string]any
}

const (
	keyMarker  = "PRIVATE-KEY-MARKER-7f3a"
	sockMarker = "agent-sock-marker-91c2"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRemoteRig(t *testing.T) *remoteRig {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &remoteRig{home: filepath.Join(root, "home"), sshLog: filepath.Join(root, "ssh.log"), holdsNew: true}
	remotes := filepath.Join(root, "remotes")
	r.bare = filepath.Join(remotes, "team", "app.git")
	for _, d := range []string{r.home, filepath.Join(r.home, ".ssh"), filepath.Dir(r.bare)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", r.home)
	// Neither the rig's git nor host git reads this machine's system
	// configuration (its hooks, helpers or proxy).
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	testEnv = []string{"GIT_CONFIG_NOSYSTEM=1"}
	t.Cleanup(func() { testEnv = nil })
	cloneDir := filepath.Join(root, "abhed-clone")
	if err := os.MkdirAll(cloneDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The rig's home is in the temp folder; the checks name their own areas.
	oldAreas := writableAreas
	writableAreas = func() []string { return nil }
	t.Cleanup(func() { writableAreas = oldAreas })
	oldParent, oldRelay := cloneParent, relayParent
	cloneParent = func() (string, error) { return cloneDir, nil }
	relayParent = func() (string, error) { return cloneDir, nil }
	runDirFolder := filepath.Join(root, "git-run")
	if err := os.MkdirAll(runDirFolder, 0o700); err != nil {
		t.Fatal(err)
	}
	oldRun := runParent
	runParent = func() (string, error) { return runDirFolder, nil }
	t.Cleanup(func() { runParent = oldRun })
	t.Cleanup(func() { cloneParent, relayParent = oldParent, oldRelay })
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(r.home, ".config"))
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(root, sockMarker))
	key := filepath.Join(r.home, ".ssh", "id_ed25519")
	if err := os.WriteFile(key, []byte(keyMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The person's ssh: it reads their key and agent, as ssh does, and runs
	// the remote command against the local bare repositories.
	stub := filepath.Join(root, "ssh-stub")
	script := "#!/bin/sh\n" +
		"for a; do last=$a; done\n" +
		"k=$(cat " + key + ")\n" +
		"[ -n \"$k\" ] || exit 9\n" +
		"echo \"host=$1 agent=${SSH_AUTH_SOCK:+set} cmd=$last\" >> " + r.sshLog + "\n" +
		"cd " + remotes + " && eval \"$last\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	global := "[core]\n\tsshCommand = " + stub + "\n[ssh]\n\tvariant = simple\n[user]\n\tname = Person\n\temail = person@example.test\n" +
		"[init]\n\tdefaultBranch = main\n"
	if err := os.WriteFile(filepath.Join(r.home, ".gitconfig"), []byte(global), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, root, "init", "-q", "--bare", "-b", "main", r.bare)
	seed := filepath.Join(root, "seed")
	run(t, root, "init", "-q", "-b", "main", seed)
	run(t, seed, "commit", "-q", "--allow-empty", "-m", "one")
	run(t, seed, "push", "-q", r.bare, "main")
	r.ws = filepath.Join(root, "ws")
	run(t, root, "clone", "-q", r.bare, r.ws)
	run(t, r.ws, "remote", "set-url", "origin", "git@git.example:team/app.git")
	// Another commit lands on the remote, for a fetch to bring.
	run(t, seed, "commit", "-q", "--allow-empty", "-m", "two")
	run(t, seed, "push", "-q", r.bare, "main")
	return r
}

func (r *remoteRig) command(ctx context.Context, cwd, line string) *exec.Cmd {
	r.mu.Lock()
	r.inner = append(r.inner, line)
	run := r.runInner
	r.mu.Unlock()
	if !run {
		return exec.CommandContext(ctx, "true")
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", line)
	cmd.Dir = cwd
	// As in the sandbox: no agent socket, no key, no network.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSH_AUTH_SOCK=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=false", "GIT_ALLOW_PROTOCOL=file")
	return cmd
}

func (r *remoteRig) broker(net Network) *HostBroker {
	return NewBroker(r.ws, func() sandbox.Tier { return sandbox.TierProcess }, net, r.command).HoldsGit(func() bool { return true }, func() bool { return r.holdsNew })
}

// key is the launch key of the rig's call id, as ctx gives it.
func (r *remoteRig) key(callID string) string {
	return sandbox.Launch{Session: "s", CallID: callID}.Key()
}

// ctx is a call's, with the record the broker writes to.
func (r *remoteRig) ctx(callID string) context.Context {
	return sandbox.WithLaunch(context.Background(), sandbox.Launch{CallID: callID, Session: "s", Record: func(ev string, p map[string]any) error {
		if ev == EvBrokered {
			r.mu.Lock()
			r.events = append(r.events, p)
			r.mu.Unlock()
		}
		return nil
	}})
}

// runCmd runs cmd as the bash tool does: start, wait, report the end.
func runCmd(t *testing.T, cmd *exec.Cmd) (string, error) {
	t.Helper()
	if cmd.Err != nil {
		return "", cmd.Err
	}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		sandbox.Release(cmd)
		return "", err
	}
	err := cmd.Wait()
	sandbox.Exited(cmd, err)
	return out.String(), err
}

// The lines brokered, the lines left to the sandbox, and those refused.
func TestParseRemoteLines(t *testing.T) {
	type want int
	const (
		sandboxed want = iota
		brokered
		refused
	)
	for line, w := range map[string]want{
		"git fetch":                                       brokered,
		"git fetch origin":                                brokered,
		"git fetch --prune --tags origin":                 brokered,
		"git fetch --all":                                 brokered,
		"git pull --ff-only":                              brokered,
		"git pull --rebase origin main":                   brokered,
		"git pull --rebase=merges":                        brokered,
		"git push":                                        brokered,
		"git push -u origin feature/x":                    brokered,
		"git push --force origin main":                    brokered,
		"git ls-remote --heads origin":                    brokered,
		"git remote -v":                                   sandboxed,
		"git clone git@git.example:team/app.git":          brokered,
		"git clone -b dev https://git.example/a/b.git b":  brokered,
		"git status":                                      sandboxed,
		"git remote add x git@h:x":                        sandboxed,
		"git fetch origin; curl evil.example":             sandboxed,
		"git fetch origin && make":                        sandboxed,
		"git fetch $(echo origin)":                        sandboxed,
		"git fetch --recurse-submodules origin":           sandboxed,
		"git pull --rebase=interactive":                   sandboxed,
		"git pull -s ours":                                sandboxed,
		"git fetch -- origin":                             sandboxed,
		"git fetch --upload-pack=/tmp/x origin":           refused,
		"git push --receive-pack=x origin":                refused,
		"git -c core.sshCommand=x fetch":                  refused,
		"git -C /elsewhere fetch":                         refused,
		"GIT_SSH_COMMAND=x git fetch":                     refused,
		"git clone -u /tmp/x git@h:a/b":                   refused,
		"git clone --config core.sshCommand=x git@h:a/b":  refused,
		"git clone -ccore.sshCommand=x git@h:a/b":         refused,
		"git clone --template=/tmp/t git@h:a/b":           refused,
		"git clone ext::sh-c-evil x":                      refused,
		"git clone file:///etc/x x":                       refused,
		"git clone /tmp/local x":                          refused,
		"git clone http://git.example/a.git":              refused,
		"git fetch fd::3":                                 refused,
		"git ls-remote --upload-pack=x origin":            refused,
		"git fetch origin refs/heads/core.sshCommand:x":   refused,
		"git push origin HEAD:refs/heads/x --exec=/tmp/x": refused,
	} {
		_, err, ok := parseRemote(line)
		got := sandboxed
		switch {
		case ok && err != nil:
			got = refused
		case ok:
			got = brokered
		}
		if got != w {
			t.Errorf("%q: got %d, want %d (err %v)", line, got, w, err)
		}
		if Brokered(line) != (w != sandboxed) {
			t.Errorf("%q: Brokered %v", line, Brokered(line))
		}
	}
}

// git fetch, pull and push run through host git with the person's own SSH
// (their global core.sshCommand, key and agent), never the sandbox, and
// each is recorded with its remote and the refs it changed.
func TestBrokerFetchPullPushThroughHostGit(t *testing.T) {
	r := newRemoteRig(t)
	r.runInner = true
	b := r.broker(Network{Open: true})
	before := run(t, r.ws, "rev-parse", "origin/main")

	out, err := runCmd(t, b.Command(r.ctx("c-fetch"), r.ws, "git fetch origin"))
	if err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	after := run(t, r.ws, "rev-parse", "origin/main")
	if after == before {
		t.Fatal("the fetch brought nothing")
	}
	if out, err := runCmd(t, b.Command(r.ctx("c-pull"), r.ws, "git pull --ff-only")); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	if run(t, r.ws, "rev-parse", "HEAD") != after {
		t.Fatal("the pull did not move HEAD")
	}
	run(t, r.ws, "commit", "-q", "--allow-empty", "-m", "three")
	if out, err := runCmd(t, b.Command(r.ctx("c-push"), r.ws, "git push origin main")); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if run(t, r.bare, "rev-parse", "main") != run(t, r.ws, "rev-parse", "HEAD") {
		t.Fatal("the push did not reach the remote")
	}
	// The pull's merge, and nothing else, ran in the sandbox.
	if len(r.inner) != 1 || !strings.Contains(r.inner[0], "git' 'pull' '--ff-only' '.' 'refs/remotes/origin/main'") {
		t.Fatalf("the sandbox ran %v", r.inner)
	}
	log, _ := os.ReadFile(r.sshLog)
	if strings.Count(string(log), "git.example agent=set") != 3 {
		t.Fatalf("the person's ssh was not used for each:\n%s", log)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) != 6 {
		t.Fatalf("recorded %d events, want a start and an end for each of 3: %v", len(r.events), r.events)
	}
	// The pull's fetch reached the network as its command was built; the others as they ran.
	for i, ev := range r.events {
		if (ev["before_start"] == true) != (i == 2 || i == 3) {
			t.Fatalf("event %d: before_start %v", i, ev["before_start"])
		}
	}
	fetchEnd := r.events[1]
	if fetchEnd["stage"] != "exited" || fetchEnd["command"] != "git fetch" || fetchEnd["call_id"] != "c-fetch" || fetchEnd["exit_code"] != 0 {
		t.Fatalf("the fetch's end: %v", fetchEnd)
	}
	b2, _ := json.Marshal(fetchEnd)
	if !strings.Contains(string(b2), `"refs/remotes/origin/main"`) || !strings.Contains(string(b2), after) ||
		!strings.Contains(string(b2), "git@git.example:team/app.git") {
		t.Fatalf("the fetch's change or remote is not recorded: %s", b2)
	}
}

// Nothing the person's credentials hold reaches a command's output or the
// record, and the URL a token is in is recorded without it.
func TestBrokerKeepsCredentialsOut(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	var all strings.Builder
	for _, line := range []string{"git fetch -v origin", "git ls-remote origin", "git remote -v", "git pull --ff-only -v"} {
		out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line))
		if err != nil {
			t.Fatalf("%s: %v\n%s", line, err, out)
		}
		all.WriteString(out)
	}
	rec, _ := json.Marshal(r.events)
	for _, s := range []string{all.String(), string(rec)} {
		if strings.Contains(s, keyMarker) || strings.Contains(s, sockMarker) {
			t.Fatalf("a credential reached the output or the record:\n%s", s)
		}
	}
	if got := anonymize("https://person:tok-secret@git.example/a/b.git"); strings.Contains(got, "tok-secret") || strings.Contains(got, "person") {
		t.Fatalf("anonymized %q", got)
	}
}

// A remote the repository does not have, or a URL, is not reached from the
// host: the line goes to the sandbox, which has no credentials.
func TestBrokerRefusesARemoteNotConfigured(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	for _, line := range []string{"git fetch upstream", "git push evil main", "git ls-remote other",
		"git fetch git@evil.example:x/y.git", "git pull https://evil.example/x.git main"} {
		if reason, step, _, err := b.Ask(r.key("c"), r.ws, line); err != nil || reason != "" || step != "" {
			t.Fatalf("%s asked %q %q %v", line, reason, step, err)
		}
		if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line)); err != nil {
			t.Fatalf("%s: %v", line, err)
		}
	}
	if len(r.inner) != 5 {
		t.Fatalf("the sandbox ran %v", r.inner)
	}
	if log, _ := os.ReadFile(r.sshLog); len(log) != 0 {
		t.Fatalf("host ssh ran:\n%s", log)
	}
}

// Dangerous options and a second command are never run on the host.
func TestBrokerRefusesDangerousLines(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	marker := filepath.Join(r.home, "ran")
	for _, line := range []string{
		"git fetch --upload-pack=touch" + marker + " origin",
		"git -c core.sshCommand=touch" + marker + " fetch origin",
		"GIT_SSH_COMMAND=touch" + marker + " git fetch origin",
		"git clone --config core.sshCommand=x git@git.example:team/app.git c",
		"git clone -u touch" + marker + " git@git.example:team/app.git c",
		"git clone ext::sh x",
		"git clone file://" + r.bare + " c",
		"git ls-remote --upload-pack=x origin",
	} {
		reason, _, _, err := b.Ask(r.key("c"), r.ws, line)
		if err == nil {
			t.Fatalf("%s was not refused before approval (%q)", line, reason)
		}
		if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line)); err == nil || !strings.Contains(err.Error(), "not run it on the host") &&
			!strings.Contains(err.Error(), "only as the plain command") && !strings.Contains(err.Error(), "only ssh and https") {
			t.Fatalf("%s: %v", line, err)
		}
	}
	// A second command leaves the whole line to the sandbox.
	for _, line := range []string{"git fetch origin; touch " + marker, "git fetch origin && touch " + marker, "git fetch origin | tee x"} {
		if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line)); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.inner) != 3 {
		t.Fatalf("the sandbox ran %v", r.inner)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused line ran its program")
	}
	if log, _ := os.ReadFile(r.sshLog); len(log) != 0 {
		t.Fatalf("host ssh ran:\n%s", log)
	}
}

// The repository's own configuration cannot change the SSH command or the
// credential helpers host git runs, and a hook or remote helper there stops
// the broker.
func TestBrokerIgnoresTheRepositorysPrograms(t *testing.T) {
	r := newRemoteRig(t)
	planted := filepath.Join(r.home, "planted-ran")
	hooks := filepath.Join(r.home, "hooks")
	writeHook(t, hooks, "pre-push", filepath.Join(r.home, "hook-ran"))
	// The person has a pre-push hook: the push's configuration must still be whole.
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	for _, kv := range [][2]string{{"core.sshCommand", "touch " + planted + ";"}, {"credential.helper", "!touch " + planted},
		{"credential.https://git.example.helper", "!touch " + planted}, {"core.askPass", "touch " + planted},
		{"core.gitProxy", "touch " + planted}} {
		run(t, r.ws, "config", kv[0], kv[1])
		b := r.broker(Network{Open: true})
		for _, line := range []string{"git fetch origin", "git push origin main"} {
			if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line)); err == nil || !strings.Contains(err.Error(), "with your credentials") {
				t.Fatalf("%s with %s set: %v", line, kv[0], err)
			}
		}
		run(t, r.ws, "config", "--unset-all", kv[0])
	}
	if _, err := os.Stat(planted); err == nil {
		t.Fatal("host git ran the repository's ssh command or helper")
	}
	if log, _ := os.ReadFile(r.sshLog); strings.Count(string(log), "git.example agent=set") != 1 {
		t.Fatalf("host ssh ran for a refused line:\n%s", log)
	}

	run(t, r.ws, "config", "hook.x.command", "touch "+planted)
	b = r.broker(Network{Open: true})
	if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch origin")); err == nil || !strings.Contains(err.Error(), "names a hook") {
		t.Fatalf("a repository naming a hook: %v", err)
	}
	run(t, r.ws, "config", "--unset", "hook.x.command")
	run(t, r.ws, "config", "remote.origin.vcs", "evil")
	b = r.broker(Network{Open: true})
	if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch origin")); err == nil || !strings.Contains(err.Error(), "remote helper") {
		t.Fatalf("a repository naming a remote helper: %v", err)
	}
}

// A remote added or changed during the session is asked about each time,
// and a call runs only on the settings it was approved on.
func TestBrokerAsksForARemoteAddedDuringTheSession(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	if reason, step, _, err := b.Ask(r.key("c-0"), r.ws, "git fetch origin"); err != nil || reason != "" || step != "" {
		t.Fatalf("an unchanged remote with the network open asked %q %q %v", reason, step, err)
	}
	run(t, r.ws, "remote", "add", "other", "git@git.example:team/app.git")
	reason, step, _, err := b.Ask(r.key("c-1"), r.ws, "git fetch other")
	if err != nil || step != StepHostGitEach || !strings.Contains(reason, "changed during this session") {
		t.Fatalf("asked %q %q %v", reason, step, err)
	}
	// Built for no judged call, it is refused.
	if _, err := runCmd(t, b.Command(r.ctx("c-unjudged"), r.ws, "git fetch other")); err == nil || !strings.Contains(err.Error(), "needs your approval") {
		t.Fatalf("an unjudged call: %v", err)
	}
	// Approved, it runs once.
	if out, err := runCmd(t, b.Command(r.ctx("c-1"), r.ws, "git fetch other")); err != nil {
		t.Fatalf("approved: %v\n%s", err, out)
	}
	// Changed after the approval, it does not run.
	if _, _, _, err := b.Ask(r.key("c-2"), r.ws, "git fetch other"); err != nil {
		t.Fatal(err)
	}
	run(t, r.ws, "remote", "set-url", "other", "git@evil.example:x/y.git")
	if _, err := runCmd(t, b.Command(r.ctx("c-2"), r.ws, "git fetch other")); err == nil || !strings.Contains(err.Error(), "changed after the call was approved") {
		t.Fatalf("changed after approval: %v", err)
	}
}

// With the network off, a brokered fetch is asked about, saying so, and
// reaches only the configured remote; under the allowlist the remote's host
// must be allowed.
func TestBrokerNetworkPolicy(t *testing.T) {
	r := newRemoteRig(t)
	off := r.broker(Network{})
	reason, step, _, err := off.Ask(r.key("c-1"), r.ws, "git fetch")
	if err != nil || step != StepHostGit || !strings.Contains(reason, "network is off") || !strings.Contains(reason, "git.example") {
		t.Fatalf("network off asked %q %q %v", reason, step, err)
	}
	if out, err := runCmd(t, off.Command(r.ctx("c-1"), r.ws, "git fetch")); err != nil {
		t.Fatalf("approved: %v\n%s", err, out)
	}
	if _, err := runCmd(t, off.Command(r.ctx("c-x"), r.ws, "git fetch")); err == nil {
		t.Fatal("an unapproved fetch ran with the network off")
	}
	// remote -v reaches nothing and is not asked.
	if reason, _, _, err := off.Ask(r.key("c-2"), r.ws, "git remote -v"); err != nil || reason != "" {
		t.Fatalf("remote -v asked %q %v", reason, err)
	}

	deny := r.broker(Network{Allows: func(host string, port uint16) (bool, string) { return false, "no rule allows " + host }})
	if _, _, _, err := deny.Ask(r.key("c-3"), r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "allowlist does not allow git.example:22") {
		t.Fatalf("allowlist: %v", err)
	}
	if _, err := runCmd(t, deny.Command(r.ctx("c-3"), r.ws, "git fetch")); err == nil {
		t.Fatal("a host the allowlist refuses was reached")
	}
	allow := r.broker(Network{Allows: func(host string, port uint16) (bool, string) { return host == "git.example" && port == 22, "" }})
	if reason, _, _, err := allow.Ask(r.key("c-4"), r.ws, "git fetch"); err != nil || reason != "" {
		t.Fatalf("an allowed host asked %q %v", reason, err)
	}
}

// A push that forces or deletes is asked each time; the family a person
// may keep for the repository never covers it.
func TestBrokerForcePushAsksEachTime(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	for _, line := range []string{"git push --force origin main", "git push origin +main", "git push origin :old", "git push -d origin old"} {
		reason, step, _, err := b.Ask(r.key("c"), r.ws, line)
		if err != nil || step != StepHostGitEach || !strings.Contains(reason, "asked each time") {
			t.Fatalf("%s asked %q %q %v", line, reason, step, err)
		}
		if f := RemoteFamily(line); f != "" {
			t.Fatalf("%s is in family %q", line, f)
		}
	}
	for line, want := range map[string]string{"git fetch": FamilyFetch, "git pull --ff-only origin main": FamilyFetch,
		"git push origin main": FamilyPush, "git clone git@git.example:a/b.git": "", "git fetch git@x:a/b": "", "git status": ""} {
		if got := RemoteFamily(line); got != want {
			t.Errorf("%s: family %q, want %q", line, got, want)
		}
	}
}

// A clone of an approved URL runs on the host into the workspace and is
// asked each time; outside the workspace it is refused, and the clone's own
// remote is then one host git reaches.
func TestBrokerClone(t *testing.T) {
	r := newRemoteRig(t)
	r.runInner = true
	b := r.broker(Network{Open: true})
	line := "git clone git@git.example:team/app.git copy"
	reason, step, _, err := b.Ask(r.key("c-1"), r.ws, line)
	if err != nil || step != StepHostGitEach || !strings.Contains(reason, "git clone") {
		t.Fatalf("clone asked %q %q %v", reason, step, err)
	}
	if out, err := runCmd(t, b.Command(r.ctx("c-1"), r.ws, line)); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	copyDir := filepath.Join(r.ws, "copy")
	if run(t, copyDir, "rev-parse", "HEAD") != run(t, r.bare, "rev-parse", "main") {
		t.Fatal("the clone is not of the remote")
	}
	// The clone's checkout, and nothing else, ran in the sandbox.
	if out, err := runCmd(t, b.Command(r.ctx("c-2"), copyDir, "git fetch")); err != nil || len(r.inner) != 1 || !strings.Contains(r.inner[0], "reset -q --hard") {
		t.Fatalf("a fetch in the clone: %v %v\n%s", err, r.inner, out)
	}
	if _, _, _, err := b.Ask(r.key("c-3"), r.ws, "git clone git@git.example:team/app.git ../outside"); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("a clone outside the workspace: %v", err)
	}
	// Unjudged, a clone is not run.
	if _, err := runCmd(t, b.Command(r.ctx("c-4"), r.ws, "git clone git@git.example:team/app.git other")); err == nil {
		t.Fatal("an unapproved clone ran")
	}
}

// The container tier leaves every line to its sandbox, refused ones too.
func TestBrokerLeavesContainersAlone(t *testing.T) {
	r := newRemoteRig(t)
	b := NewBroker(r.ws, func() sandbox.Tier { return sandbox.TierContainer }, Network{Open: true}, r.command)
	for _, line := range []string{"git fetch", "git -c core.sshCommand=x fetch"} {
		if reason, _, _, err := b.Ask(r.key("c"), r.ws, line); reason != "" || err != nil {
			t.Fatalf("%s: %q %v", line, reason, err)
		}
		if _, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line)); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.inner) != 2 {
		t.Fatalf("the sandbox ran %v", r.inner)
	}
}

// The person's name and email are read for the tiers whose commands do not
// see their configuration, and an include git cannot read loses only itself.
func TestIdentity(t *testing.T) {
	r := newRemoteRig(t)
	want := []string{"GIT_AUTHOR_NAME=Person", "GIT_COMMITTER_NAME=Person",
		"GIT_AUTHOR_EMAIL=person@example.test", "GIT_COMMITTER_EMAIL=person@example.test"}
	if got := Identity(context.Background(), r.ws); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("identity %v", got)
	}
	// The repository's own name wins, as it would for git there.
	run(t, r.ws, "config", "user.name", "Repo Person")
	if got := Identity(context.Background(), r.ws); !slices.Contains(got, "GIT_AUTHOR_NAME=Repo Person") {
		t.Fatalf("identity %v", got)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	// An include this account cannot read: git refuses every configuration file.
	locked := filepath.Join(r.home, "locked.inc")
	if err := os.WriteFile(locked, []byte("[user]\n\temail = locked@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readable := filepath.Join(r.home, "work.inc")
	if err := os.WriteFile(readable, []byte("[user]\n\temail = work@example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(r.home, ".gitconfig"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("[include]\n\tpath = " + locked + "\n")
	_ = f.Close()
	run(t, r.ws, "config", "include.path", readable)
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })
	if err := exec.Command("git", "-C", r.ws, "config", "--get", "user.name").Run(); err == nil {
		t.Skip("this git reads past an unreadable include")
	}
	got := Identity(context.Background(), r.ws)
	if !slices.Contains(got, "GIT_AUTHOR_NAME=Repo Person") || !slices.Contains(got, "GIT_AUTHOR_EMAIL=work@example.test") {
		t.Fatalf("identity with an unreadable include %v", got)
	}
	for _, kv := range got {
		if strings.Contains(kv, "locked@") {
			t.Fatalf("read the unreadable include: %v", got)
		}
	}
}

// A name with a control character is not passed on.
func TestIdentityPrintable(t *testing.T) {
	for s, want := range map[string]bool{"Ana": true, "": false, " ": false, "a\nGIT_DIR=/x": false, "a\x1b[2J": false} {
		if printable(s) != want {
			t.Errorf("printable(%q) = %v", s, !want)
		}
	}
}

// writeHook writes an executable hook into dir that touches marker.
func writeHook(t *testing.T, dir, name, marker string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// pushReady pulls the remote's new commit and adds one of the workspace's,
// so a push fast-forwards.
func (r *remoteRig) pushReady(t *testing.T, b *HostBroker) {
	t.Helper()
	r.runInner = true
	if out, err := runCmd(t, b.Command(r.ctx("c-pull"), r.ws, "git pull -q --ff-only")); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	run(t, r.ws, "-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty", "-m", "three")
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
}

// A brokered push runs the person's own pre-push hook (their global or
// system core.hooksPath) in the sandbox, never on the host, with git's
// arguments and input, and records that it ran there; the repository's own
// hooks, in .git/hooks or a core.hooksPath of its own, never run. A fetch
// runs no hook, and records the folder it did not run.
func TestBrokerRunsThePersonsPrePushInTheSandbox(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	got := filepath.Join(r.home, "pre-push-saw")
	repoRan, gitRan := filepath.Join(r.home, "repo-hook-ran"), filepath.Join(r.home, "githooks-ran")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\n{ echo \"args=$1 $2\"; cat; echo \"sock=$SSH_AUTH_SOCK\"; } > "+got+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHook(t, filepath.Join(r.ws, "repo-hooks"), "pre-push", repoRan)
	writeHook(t, filepath.Join(r.ws, ".git", "hooks"), "pre-push", gitRan)
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	run(t, r.ws, "config", "core.hooksPath", filepath.Join(r.ws, "repo-hooks"))
	b = r.broker(Network{Open: true})
	r.mu.Lock()
	r.inner = nil
	r.mu.Unlock()
	if out, err := runCmd(t, b.Command(r.ctx("c-push"), r.ws, "git push origin main")); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	saw, err := os.ReadFile(got)
	if err != nil {
		t.Fatal("the person's pre-push hook did not run")
	}
	head := run(t, r.ws, "rev-parse", "HEAD")
	if !strings.Contains(string(saw), "args=origin git@git.example:team/app.git") ||
		!strings.Contains(string(saw), "refs/heads/main "+head+" refs/heads/main ") || strings.Contains(string(saw), sockMarker) {
		t.Fatalf("the hook saw:\n%s", saw)
	}
	r.mu.Lock()
	inner := strings.Join(r.inner, "\n")
	r.mu.Unlock()
	if !strings.Contains(inner, filepath.Join(hooks, "pre-push")) {
		t.Fatalf("the hook did not run in the sandbox: %q", inner)
	}
	for _, m := range []string{repoRan, gitRan} {
		if _, err := os.Stat(m); err == nil {
			t.Fatalf("a hook of the repository's ran (%s)", m)
		}
	}
	r.mu.Lock()
	rec, _ := json.Marshal(r.events)
	r.events = nil
	r.mu.Unlock()
	if !strings.Contains(string(rec), `"hooks_run":[{"hook":"pre-push","path":"`+hooks+`","scope":"global","where":"sandbox"}]`) ||
		!strings.Contains(string(rec), `"pre_push_exit_code":0`) {
		t.Fatalf("the record: %s", rec)
	}
	if out, err := runCmd(t, b.Command(r.ctx("c-fetch"), r.ws, "git fetch")); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	r.mu.Lock()
	rec, _ = json.Marshal(r.events)
	r.mu.Unlock()
	if !strings.Contains(string(rec), `"hooks_not_run":[{"path":"`+hooks+`","scope":"global"}]`) || strings.Contains(string(rec), "hooks_run") {
		t.Fatalf("the fetch's record: %s", rec)
	}
}

// A pre-push hook that fails in the sandbox stops the push, and its output
// is shown.
func TestBrokerPrePushFailureStopsThePush(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\necho 'secret found in app.env' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	before := run(t, r.bare, "rev-parse", "main")
	out, err := runCmd(t, b.Command(r.ctx("c-push"), r.ws, "git push origin main"))
	if err == nil || !strings.Contains(out, "secret found in app.env") {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if run(t, r.bare, "rev-parse", "main") != before {
		t.Fatal("the push went on past its failed hook")
	}
	r.mu.Lock()
	rec, _ := json.Marshal(r.events)
	r.mu.Unlock()
	if !strings.Contains(string(rec), `"pre_push_exit_code":3`) {
		t.Fatalf("the record: %s", rec)
	}
}

// Whatever host git or the person's ssh prints, a user and password in a
// URL never reach the output.
func TestBrokerRedactsURLCredentials(t *testing.T) {
	r := newRemoteRig(t)
	root := filepath.Dir(r.ws)
	stub := filepath.Join(root, "ssh-stub")
	noisy := filepath.Join(root, "ssh-noisy")
	if err := os.WriteFile(noisy, []byte("#!/bin/sh\necho 'see https://oauth2:STUB-TOKEN-7@git.example/x' >&2\nexec "+stub+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gc := filepath.Join(r.home, ".gitconfig")
	cfg, _ := os.ReadFile(gc)
	if err := os.WriteFile(gc, []byte(strings.Replace(string(cfg), stub, noisy, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	b := r.broker(Network{Open: true})
	out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch"))
	if err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	if strings.Contains(out, "STUB-TOKEN-7") || !strings.Contains(out, "https://git.example/x") {
		t.Fatalf("output:\n%s", out)
	}
}

// What a call was judged on is kept for that session's call alone: the same
// call id in another session of one server was never asked about.
func TestBrokerJudgementIsPerSession(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{})
	if _, _, _, err := b.Ask(sandbox.Launch{Session: "s1", CallID: "c"}.Key(), r.ws, "git fetch"); err != nil {
		t.Fatal(err)
	}
	other := sandbox.WithLaunch(context.Background(), sandbox.Launch{Session: "s2", CallID: "c"})
	if _, err := runCmd(t, b.Command(other, r.ws, "git fetch")); err == nil || !strings.Contains(err.Error(), "needs your approval") {
		t.Fatalf("another session's call ran on this one's approval: %v", err)
	}
}

// Where the sandbox binds git's files as each command starts, a repository
// cloned during the session is not brokered until the next: a command
// started before the clone could still write its configuration.
func TestBrokerWaitsOnASessionsCloneWhereBound(t *testing.T) {
	r := newRemoteRig(t)
	r.runInner, r.holdsNew = true, false
	b := r.broker(Network{Open: true})
	if _, _, _, err := b.Ask(r.key("c-1"), r.ws, "git clone git@git.example:team/app.git copy"); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, b.Command(r.ctx("c-1"), r.ws, "git clone git@git.example:team/app.git copy")); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	copyDir := filepath.Join(r.ws, "copy")
	if _, _, _, err := b.Ask(r.key("c-2"), copyDir, "git fetch"); err == nil || !strings.Contains(err.Error(), "when it is that session's workspace") {
		t.Fatalf("a fetch in this session's clone: %v", err)
	}
	// The next session's broker knows it from its start.
	next := NewBroker(copyDir, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, r.command).
		HoldsGit(func() bool { return true }, func() bool { return false })
	if _, _, _, err := next.Ask(r.key("c-3"), copyDir, "git fetch"); err != nil {
		t.Fatalf("the next session: %v", err)
	}
}

// A pre-push hook the person has but the sandbox cannot run stops the push:
// it is never skipped unseen.
func TestBrokerPrePushTheSandboxCannotRunStopsThePush(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	writeHook(t, hooks, "pre-push", filepath.Join(r.home, "ran"))
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	b := r.broker(Network{Open: true})
	r.pushReady(t, b)
	before := run(t, r.bare, "rev-parse", "main")
	// The rig's sandbox cannot run it.
	r.mu.Lock()
	r.runInner = false
	r.mu.Unlock()
	// It runs the line as given, but cannot see the person's hook folder.
	failing := func(ctx context.Context, cwd, line string) *exec.Cmd {
		r.mu.Lock()
		r.inner = append(r.inner, line)
		r.mu.Unlock()
		cmd := exec.CommandContext(ctx, "bash", "-c", strings.ReplaceAll(line, hooks, filepath.Join(r.home, "not-visible")))
		cmd.Dir = cwd
		return cmd
	}
	b2 := NewBroker(r.ws, func() sandbox.Tier { return sandbox.TierProcess }, Network{Open: true}, failing).
		HoldsGit(func() bool { return true }, func() bool { return true })
	out, err := runCmd(t, b2.Command(r.ctx("c-push"), r.ws, "git push origin main"))
	if err == nil || !strings.Contains(out, "not-visible") {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if run(t, r.bare, "rev-parse", "main") != before {
		t.Fatal("the push went on without its hook")
	}
}

// The person's configuration is trusted as theirs only where no command can
// write it: an include in a folder the sandbox writes, or one git cannot
// read (so what it brings is not known), stops the broker, clones too.
func TestBrokerNeedsThePersonsConfigOutOfReach(t *testing.T) {
	r := newRemoteRig(t)
	area := filepath.Join(r.home, "writable")
	if err := os.MkdirAll(area, 0o700); err != nil {
		t.Fatal(err)
	}
	inc := filepath.Join(area, "extra.gitconfig")
	if err := os.WriteFile(inc, []byte("[user]\n\tname = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, r.home, "config", "--global", "include.path", inc)
	b := r.broker(Network{Open: true})
	if _, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch"); err != nil {
		t.Fatalf("an include outside every writable area: %v", err)
	}
	writableAreas = func() []string { return []string{area} }
	for _, line := range []string{"git fetch", "git clone git@git.example:team/app.git copy"} {
		if _, _, _, err := b.Ask(r.key("c"), r.ws, line); err == nil || !strings.Contains(err.Error(), "which commands can write") {
			t.Fatalf("%s with an include in a writable folder: %v", line, err)
		}
	}
	writableAreas = func() []string { return nil }
	if os.Geteuid() != 0 {
		if err := os.Chmod(inc, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(inc, 0o600) })
		if _, _, _, err := b.Ask(r.key("c"), r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "could not be read") {
			t.Fatalf("an unreadable include: %v", err)
		}
	}
}

// A GIT_CONFIG_GLOBAL the person set is the configuration host git uses,
// as their own git would, once it is checked as theirs.
func TestBrokerHonoursGitConfigGlobal(t *testing.T) {
	r := newRemoteRig(t)
	gc := filepath.Join(r.home, ".gitconfig")
	own := filepath.Join(r.home, "own.gitconfig")
	if err := os.Rename(gc, own); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", own)
	b := r.broker(Network{Open: true})
	if out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch")); err != nil {
		t.Fatalf("fetch with GIT_CONFIG_GLOBAL: %v\n%s", err, out)
	}
	if log, _ := os.ReadFile(r.sshLog); !strings.Contains(string(log), "git.example agent=set") {
		t.Fatalf("the person's ssh, from GIT_CONFIG_GLOBAL, did not run:\n%s", log)
	}
}

// A configuration file, or its folder, other users can write is not the
// person's alone.
func TestBrokerRefusesAConfigOthersCanWrite(t *testing.T) {
	r := newRemoteRig(t)
	gc := filepath.Join(r.home, ".gitconfig")
	for _, c := range []struct {
		path string
		mode os.FileMode
		why  string
	}{{gc, 0o622, "writable by other users"}, {r.home, 0o777, "folder other users can write"}} {
		info, err := os.Stat(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(c.path, c.mode); err != nil {
			t.Fatal(err)
		}
		b := r.broker(Network{Open: true})
		_, _, _, err = b.Ask(r.key("c"), r.ws, "git fetch")
		_ = os.Chmod(c.path, info.Mode().Perm())
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Fatalf("%s mode %o: %v", c.path, c.mode, err)
		}
	}
}

// A server's broker never runs remote git on the host: every remote line
// goes to the sandbox, asked about by no one, while worktree commands are
// still brokered. A broker the configuration turns off refuses with why.
func TestBrokerNoRemotesAndRemotesOff(t *testing.T) {
	r := newRemoteRig(t)
	served := r.broker(Network{}).NoRemotes()
	for _, line := range []string{"git fetch", "git push origin main", "git clone git@git.example:team/app.git c", "git ls-remote origin"} {
		if reason, _, _, err := served.Ask(r.key("c"), r.ws, line); reason != "" || err != nil {
			t.Fatalf("%s was asked about on a server: %q %v", line, reason, err)
		}
		if _, err := runCmd(t, served.Command(r.ctx("c"), r.ws, line)); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.inner) != 4 || served.BrokersRemoteCommands() {
		t.Fatalf("the sandbox ran %v", r.inner)
	}
	if log, _ := os.ReadFile(r.sshLog); len(log) != 0 {
		t.Fatalf("host ssh ran for a server:\n%s", log)
	}
	if !Brokered("git worktree prune") {
		t.Fatal("worktree commands are no longer the broker's")
	}
	// Turned off by the configuration, remote lines run in the sandbox, as in
	// 1.2.9, asked about by no one; the session facts say why.
	off := r.broker(Network{Open: true}).RemotesOff("ask your administrator")
	if reason, _, _, err := off.Ask(r.key("c"), r.ws, "git fetch"); reason != "" || err != nil {
		t.Fatalf("turned off, it was asked about: %q %v", reason, err)
	}
	if _, err := runCmd(t, off.Command(r.ctx("c"), r.ws, "git fetch")); err != nil {
		t.Fatalf("turned off: %v", err)
	}
	if len(r.inner) != 5 || off.BrokersRemoteCommands() {
		t.Fatalf("turned off, the sandbox ran %v", r.inner)
	}
	if log, _ := os.ReadFile(r.sshLog); len(log) != 0 {
		t.Fatalf("host ssh ran while turned off:\n%s", log)
	}
}

// A plain git push the repository's own configuration makes a mirror, a
// forcing or a deleting push is asked each time, as one, never answered by
// the family a person keeps; and ImpliedLine says what it runs as, so policy
// judges that too.
func TestBrokerConfigImpliedPushAsksAsDestructive(t *testing.T) {
	for _, c := range []struct {
		set  [][2]string
		want string
	}{
		{[][2]string{{"remote.origin.mirror", "true"}}, "git push --force --mirror origin"},
		{[][2]string{{"remote.origin.push", ":refs/heads/x"}}, "git push --delete origin"},
		{[][2]string{{"remote.origin.push", "+refs/heads/*:refs/heads/*"}}, "git push --force origin"},
		{[][2]string{{"remote.origin.push", "refs/heads/main:refs/heads/main"}}, ""},
		{[][2]string{{"remote.origin.mirror", "false"}}, ""},
	} {
		r := newRemoteRig(t)
		b := r.broker(Network{Open: true})
		for _, kv := range c.set {
			if out, err := exec.Command("git", "-C", r.ws, "config", "--add", kv[0], kv[1]).CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		}
		if got := ImpliedLine(r.ws, "git push"); got != c.want {
			t.Fatalf("%v: implied %q, want %q", c.set, got, c.want)
		}
		reason, step, _, err := b.Ask(r.key("c"), r.ws, "git push origin main")
		if c.want == "" {
			continue
		}
		if err != nil || step != StepHostGitEach || !strings.Contains(reason, "repository's own configuration") {
			t.Fatalf("%v: asked %q %q %v", c.set, reason, step, err)
		}
		if got := ImpliedLine(r.ws, "git push origin main"); !strings.HasPrefix(got, "git push --") || !strings.HasSuffix(got, "origin main") {
			t.Fatalf("%v: implied for a named remote %q", c.set, got)
		}
	}
	if ImpliedLine(t.TempDir(), "git push") != "" || ImpliedLine(t.TempDir(), "git status") != "" {
		t.Fatal("implied outside a repository")
	}
}

// What a push sends is bound into the grant's scope: once a session sets
// remote.origin.mirror or a push refspec, a later session's push is bound to
// another scope, so a grant kept before no longer answers it. A remote with
// neither keeps the scope it had.
func TestPushScopeBindsMirrorAndPushRefspecs(t *testing.T) {
	r := newRemoteRig(t)
	bind := func(key string) (string, string) {
		_, step, b, err := r.broker(Network{}).Ask(r.key(key), r.ws, "git push origin main")
		if err != nil {
			t.Fatal(err)
		}
		return step, b
	}
	step, before := bind("p-0")
	if step != StepHostGit || before == "" {
		t.Fatalf("a push with the network off: step %q bind %q", step, before)
	}
	if _, again := bind("p-1"); again != before {
		t.Fatal("an unchanged remote's scope changed")
	}
	run(t, r.ws, "config", "remote.origin.mirror", "true")
	if _, mirrored := bind("p-2"); mirrored == before {
		t.Fatal("a mirror set since leaves the scope a kept push grant answers")
	}
	run(t, r.ws, "config", "--unset", "remote.origin.mirror")
	run(t, r.ws, "config", "remote.origin.push", ":refs/heads/main")
	if _, deleting := bind("p-3"); deleting == before {
		t.Fatal("a deleting push refspec set since leaves the scope a kept push grant answers")
	}
}
