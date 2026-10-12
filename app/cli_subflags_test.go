package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// The top-level flags written after a subcommand are taken when the
// subcommand reads them, refused when it does not, and left alone where
// they are the subcommand's own or part of a command passed on as given.
func TestHoistGlobalFlags(t *testing.T) {
	for _, c := range []struct {
		args    []string
		left    string
		workdir string
		trust   bool
		set     string
		err     string
	}{
		{args: []string{"doctor", "-C", "/ws", "--json"}, left: "doctor --json", workdir: "/ws"},
		{args: []string{"doctor", "--json", "-settings", "s.json", "-trust-workspace"}, left: "doctor --json", set: "s.json", trust: true},
		{args: []string{"doctor", "--C=/ws"}, left: "doctor", workdir: "/ws"},
		{args: []string{"doctor", "-model", "x"}, err: "-model does not apply to doctor"},
		{args: []string{"doctor", "-C"}, err: "-C needs a value"},
		{args: []string{"doctor", "extra"}, err: "doctor takes only --json and -timeout; got extra"},
		{args: []string{"doctor", "-timeout", "2m", "-C", "/ws"}, left: "doctor -timeout 2m", workdir: "/ws"},
		{args: []string{"doctor", "--timeout=5s"}, left: "doctor --timeout=5s"},
		{args: []string{"doctor", "-h"}, left: "doctor -h"},
		{args: []string{"version", "-C", "/ws", "-json"}, left: "version -json", workdir: "/ws"},
		{args: []string{"acp", "-C", "/ws", "-trust-workspace"}, left: "acp", workdir: "/ws", trust: true},
		{args: []string{"acp", "foo"}, err: "acp takes no arguments; got foo"},
		{args: []string{"rpc", "--", "-trust-workspace"}, err: "rpc takes no arguments; got --"},
		{args: []string{"index", "-p"}, err: "-p does not apply to index"},
		{args: []string{"init", "-C", "/ws"}, left: "init", workdir: "/ws"},
		{args: []string{"providers", "-C", "/ws"}, err: "-C does not apply to providers"},
		{args: []string{"serve", "-addr", ":8861", "-C", "/ws", "-trust-workspace", "-check"},
			left: "serve -addr :8861 -trust-workspace -check", workdir: "/ws"},
		{args: []string{"serve", "-settings", "s.json"}, err: "-settings does not apply to serve"},
		{args: []string{"eval", "-corpus", "d", "-trust-workspace", "-C", "/ws"}, left: "eval -corpus d -trust-workspace", workdir: "/ws"},
		{args: []string{"record", "list", "-n", "5", "-C", "/ws"}, left: "record list -n 5", workdir: "/ws"},
		{args: []string{"sessions", "-n", "3"}, left: "sessions -n 3"},
		{args: []string{"hawkeye", "abc", "-o", "r.html", "-C", "/ws"}, left: "hawkeye abc -o r.html", workdir: "/ws"},
		{args: []string{"user", "add", "bob", "-name", "Bob", "-C", "/ws", "-trust-workspace"},
			left: "user add bob -name Bob", workdir: "/ws", trust: true},
		{args: []string{"resolve", "https://h/o/r/issues/1", "-mode", "plan", "-allow", "x", "-C", "/ws"},
			left: "resolve https://h/o/r/issues/1 -mode plan -allow x", workdir: "/ws"},
		{args: []string{"trust", "-C", "/ws"}, left: "trust", workdir: "/ws"},
		{args: []string{"trust", "-trust-workspace"}, err: "-trust-workspace does not apply to trust"},
		{args: []string{"migrate", "-C", "/ws", "-owners", "keep"}, left: "migrate -owners keep", workdir: "/ws"},
		{args: []string{"mcp", "list", "-C", "/ws"}, left: "mcp list", workdir: "/ws"},
		{args: []string{"mcp", "add", "git", "git", "-C", "/repo"}, left: "mcp add git git -C /repo"},
		{args: []string{"login", "https://h", "-profile", "p", "-C", "/ws"}, left: "login https://h -profile p", workdir: "/ws"},
		{args: []string{"logout", "-C", "/ws"}, left: "logout", workdir: "/ws"},
		{args: []string{"profile", "list", "-C", "/ws"}, left: "profile list", workdir: "/ws"},
		{args: []string{"sandbox", "list", "-C", "/ws"}, left: "sandbox list", workdir: "/ws"},
		{args: []string{"sandbox", "exec", "sb1", "ls", "-C", "x"}, left: "sandbox exec sb1 ls -C x"},
		{args: []string{"sandbox", "exec", "-c", "ls", "sb1"}, left: "sandbox exec -c ls sb1"},
		{args: []string{"sandbox", "snapshot", "sb1", "-name", "s"}, left: "sandbox snapshot sb1 -name s"},
		{args: []string{"pki", "init", "-name", "CA"}, left: "pki init -name CA"},
		{args: []string{"pki", "init", "-C", "/ws"}, err: "-C does not apply to pki"},
		{args: []string{"secret", "list", "-model", "x"}, err: "-model does not apply to secret"},
		{args: []string{"admin", "web-search", "on", "-C", "/ws"}, err: "-C does not apply to admin"},
	} {
		var f cliFlags
		fs := newFlagSet(&f)
		fs.SetOutput(&strings.Builder{})
		var cont bool
		var name string
		fs.BoolVar(&cont, "c", false, "")
		fs.StringVar(&name, "n", "", "")
		fs.StringVar(&name, "name", "", "")
		left, err := hoistGlobalFlags(fs, c.args)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: err %v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if got := strings.Join(left, " "); got != c.left || f.workdir != c.workdir || f.trustWS != c.trust || f.settings != c.set || name != "" || cont {
			t.Errorf("%v: left %q workdir %q trust %v settings %q name %q c %v", c.args, got, f.workdir, f.trustWS, f.settings, name, cont)
		}
	}
}

// `abhed doctor -C ws` checks ws, not the current folder; a flag doctor
// does not read stops it with exit code 2 before anything is checked.
func TestDoctorTakesWorkspaceAfterTheSubcommand(t *testing.T) {
	_, ws := trustWorkspace(t, `{"permissions":{"mode":"accept-edits"}}`)
	out, code := stdoutOf(t, func() int { return Main([]string{"doctor", "-C", ws, "-trust-workspace", "-timeout", "5s"}) })
	if !strings.Contains(out, "workspace   "+ws) || !strings.Contains(out, "mode        accept-edits") {
		t.Fatalf("exit %d; -C after doctor was not taken:\n%s", code, out)
	}
	// It names the edition and the files it judged.
	if !strings.Contains(out, "edition     Community Edition\n") ||
		!strings.Contains(out, "config      ") || !strings.Contains(out, filepath.Join(ws, ".abhed", "config.json")) {
		t.Fatalf("no edition or config line:\n%s", out)
	}
	for _, args := range [][]string{{"doctor", "-timeout", "soon"}, {"doctor", "-timeout", "0s"}} {
		if _, code := stdoutOf(t, func() int { return Main(append(args, "-C", ws)) }); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	for _, args := range [][]string{{"doctor", "-model", "x"}, {"doctor", "-C", ws, "now"}, {"index", "-C", ws, "extra"}} {
		if out, code := stdoutOf(t, func() int { return Main(args) }); code != 2 || out != "" {
			t.Errorf("%v: exit %d, output %q; want 2 and nothing run", args, code, out)
		}
	}
}
