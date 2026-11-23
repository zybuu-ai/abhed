package hostgit

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	mu                     sync.Mutex
	events                 []map[string]any
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
	r := &remoteRig{home: filepath.Join(root, "home"), sshLog: filepath.Join(root, "ssh.log")}
	remotes := filepath.Join(root, "remotes")
	r.bare = filepath.Join(remotes, "team", "app.git")
	for _, d := range []string{r.home, filepath.Join(r.home, ".ssh"), filepath.Dir(r.bare)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", r.home)
	// The rig's own git reads no system configuration (a machine's hooks);
	// host git drops the variable and keeps hooks off itself.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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
	r.mu.Unlock()
	return exec.CommandContext(ctx, "true")
}

func (r *remoteRig) broker(net Network) *HostBroker {
	return NewBroker(r.ws, func() sandbox.Tier { return sandbox.TierProcess }, net, r.command)
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
		"git remote -v":                                   brokered,
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
	if len(r.inner) != 0 {
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
		if reason, step, err := b.Ask("c", r.ws, line); err != nil || reason != "" || step != "" {
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
		reason, _, err := b.Ask("c", r.ws, line)
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
	run(t, r.ws, "config", "core.sshCommand", "touch "+planted+";")
	run(t, r.ws, "config", "credential.helper", "!touch "+planted)
	run(t, r.ws, "config", "core.askPass", "touch "+planted)
	b := r.broker(Network{Open: true})
	if out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, "git fetch origin")); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	if _, err := os.Stat(planted); err == nil {
		t.Fatal("host git ran the repository's ssh command or helper")
	}
	if log, _ := os.ReadFile(r.sshLog); !strings.Contains(string(log), "git.example agent=set") {
		t.Fatalf("the person's ssh did not run:\n%s", log)
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
	if reason, step, err := b.Ask("c-0", r.ws, "git fetch origin"); err != nil || reason != "" || step != "" {
		t.Fatalf("an unchanged remote with the network open asked %q %q %v", reason, step, err)
	}
	run(t, r.ws, "remote", "add", "other", "git@git.example:team/app.git")
	reason, step, err := b.Ask("c-1", r.ws, "git fetch other")
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
	if _, _, err := b.Ask("c-2", r.ws, "git fetch other"); err != nil {
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
	reason, step, err := off.Ask("c-1", r.ws, "git fetch")
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
	if reason, _, err := off.Ask("c-2", r.ws, "git remote -v"); err != nil || reason != "" {
		t.Fatalf("remote -v asked %q %v", reason, err)
	}

	deny := r.broker(Network{Allows: func(host string, port uint16) (bool, string) { return false, "no rule allows " + host }})
	if _, _, err := deny.Ask("c-3", r.ws, "git fetch"); err == nil || !strings.Contains(err.Error(), "allowlist does not allow git.example:22") {
		t.Fatalf("allowlist: %v", err)
	}
	if _, err := runCmd(t, deny.Command(r.ctx("c-3"), r.ws, "git fetch")); err == nil {
		t.Fatal("a host the allowlist refuses was reached")
	}
	allow := r.broker(Network{Allows: func(host string, port uint16) (bool, string) { return host == "git.example" && port == 22, "" }})
	if reason, _, err := allow.Ask("c-4", r.ws, "git fetch"); err != nil || reason != "" {
		t.Fatalf("an allowed host asked %q %v", reason, err)
	}
}

// A push that forces or deletes is asked each time; the family a person
// may keep for the repository never covers it.
func TestBrokerForcePushAsksEachTime(t *testing.T) {
	r := newRemoteRig(t)
	b := r.broker(Network{Open: true})
	for _, line := range []string{"git push --force origin main", "git push origin +main", "git push origin :old", "git push -d origin old"} {
		reason, step, err := b.Ask("c", r.ws, line)
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
	b := r.broker(Network{Open: true})
	line := "git clone git@git.example:team/app.git copy"
	reason, step, err := b.Ask("c-1", r.ws, line)
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
	if out, err := runCmd(t, b.Command(r.ctx("c-2"), copyDir, "git fetch")); err != nil || len(r.inner) != 0 {
		t.Fatalf("a fetch in the clone: %v %v\n%s", err, r.inner, out)
	}
	if _, _, err := b.Ask("c-3", r.ws, "git clone git@git.example:team/app.git ../outside"); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
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
		if reason, _, err := b.Ask("c", r.ws, line); reason != "" || err != nil {
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

// The hooks the person's own configuration names do not run on a brokered
// push, as hooks never run in host git; the record says which were not run.
func TestBrokerRecordsHooksNotRun(t *testing.T) {
	r := newRemoteRig(t)
	hooks := filepath.Join(r.home, "hooks")
	marker := filepath.Join(r.home, "hook-ran")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	b := r.broker(Network{Open: true})
	if out, err := runCmd(t, b.Command(r.ctx("c-pull"), r.ws, "git pull -q --ff-only")); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	run(t, r.ws, "commit", "-q", "--allow-empty", "--no-verify", "-m", "three")
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
	if out, err := runCmd(t, b.Command(r.ctx("c-push"), r.ws, "git push origin main")); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the person's pre-push hook ran in host git")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) != 2 {
		t.Fatalf("events %v", r.events)
	}
	for _, ev := range r.events {
		got, _ := json.Marshal(ev["hooks_not_run"])
		if !strings.Contains(string(got), `"scope":"global"`) || !strings.Contains(string(got), hooks) {
			t.Fatalf("hooks not run, as recorded: %s", got)
		}
	}
}
