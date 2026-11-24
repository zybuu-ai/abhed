package hostgit

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// configOf is the command-line configuration env gives git, key to values.
func configOf(t *testing.T, env []string) map[string][]string {
	t.Helper()
	get := func(name string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				return v
			}
		}
		return ""
	}
	n, err := strconv.Atoi(get("GIT_CONFIG_COUNT"))
	if err != nil {
		t.Fatalf("no GIT_CONFIG_COUNT")
	}
	cfg := map[string][]string{}
	for i := range n {
		k := get("GIT_CONFIG_KEY_" + strconv.Itoa(i))
		cfg[strings.ToLower(k)] = append(cfg[strings.ToLower(k)], get("GIT_CONFIG_VALUE_"+strconv.Itoa(i)))
	}
	return cfg
}

// Every command host git runs for the broker (fetch, ls-remote, a pull's
// fetch, a clone, a push with and without the person's pre-push) has one
// block of command-line configuration, no variable twice, and every pin in
// it: one more block would replace them all.
func TestHostCommandEnvIsOneBlockWithEveryPin(t *testing.T) {
	r := newRemoteRig(t)
	r.runInner = true
	var mu sync.Mutex
	var envs [][]string
	seenEnv = func(env []string) {
		mu.Lock()
		envs = append(envs, append([]string(nil), env...))
		mu.Unlock()
	}
	t.Cleanup(func() { seenEnv = nil })
	b := r.broker(Network{Open: true})
	r.pushReady(t, b) // a pull's fetch
	for _, line := range []string{"git fetch", "git ls-remote origin", "git push origin main"} {
		if out, err := runCmd(t, b.Command(r.ctx("c"), r.ws, line)); err != nil {
			t.Fatalf("%s: %v\n%s", line, err, out)
		}
	}
	hooks := filepath.Join(r.home, "hooks")
	writeHook(t, hooks, "pre-push", filepath.Join(r.home, "hook-ran"))
	run(t, r.home, "config", "--global", "core.hooksPath", hooks)
	run(t, r.ws, "-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty", "-m", "four")
	b = r.broker(Network{Open: true})
	if out, err := runCmd(t, b.Command(r.ctx("c-hook"), r.ws, "git push origin main")); err != nil {
		t.Fatalf("push with a hook: %v\n%s", err, out)
	}
	_, _, _, _ = b.Ask(r.key("c-clone"), r.ws, "git clone git@git.example:team/app.git copy")
	if out, err := runCmd(t, b.Command(r.ctx("c-clone"), r.ws, "git clone git@git.example:team/app.git copy")); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(envs) != 6 {
		t.Fatalf("saw %d host commands", len(envs))
	}
	stub := filepath.Join(filepath.Dir(r.ws), "ssh-stub")
	for i, env := range envs {
		names := map[string]bool{}
		counts := 0
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			if names[name] {
				t.Errorf("command %d names %s twice", i, name)
			}
			names[name] = true
			if name == "GIT_CONFIG_COUNT" {
				counts++
			}
		}
		if counts != 1 {
			t.Errorf("command %d has %d configuration blocks", i, counts)
		}
		cfg := configOf(t, env)
		last := func(k string) string { v := cfg[k]; return v[len(v)-1] }
		for k, want := range map[string]string{"core.sshcommand": stub, "core.askpass": "", "core.gitproxy": "", "http.proxy": "",
			"http.sslverify": "true", "core.pager": "cat", "protocol.allow": "never", "core.fsmonitor": "false", "gc.auto": "0"} {
			if len(cfg[k]) == 0 || last(k) != want {
				t.Errorf("command %d: %s is %q, want %q", i, k, cfg[k], want)
			}
		}
		if h := cfg["credential.helper"]; len(h) == 0 || h[0] != "" {
			t.Errorf("command %d: credential.helper %q is not emptied first", i, h)
		}
		if hp := last("core.hookspath"); i == 4 {
			if !strings.Contains(hp, "pre-push-") {
				t.Errorf("the push with a hook runs hooks from %q, not the relay", hp)
			}
		} else if hp != "/dev/null" {
			t.Errorf("command %d runs hooks from %q", i, hp)
		}
	}
}

// A command given configuration in its environment, beside the block its
// settings are in, is refused: it would replace them all.
func TestSecondConfigBlockIsRefused(t *testing.T) {
	if _, err := hostEnv([]string{"A=1"}, safety, "GIT_CONFIG_COUNT=1"); err == nil {
		t.Fatal("hostEnv took a second block")
	}
	if err := checkEnv([]string{"GIT_CONFIG_COUNT=2", "X=1", "GIT_CONFIG_COUNT=1"}); err == nil {
		t.Fatal("checkEnv took a name twice")
	}
	if cmd := hostScript(t.Context(), t.TempDir(), []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_COUNT=2"}, [][]string{{"status"}}); cmd.Err == nil {
		t.Fatal("hostScript ran with a name twice")
	}
	if cmd := New(t.Context(), t.TempDir()).CommandWith(t.Context(), nil, []string{"GIT_CONFIG_COUNT=0"}, "status"); cmd.Err == nil {
		t.Fatal("CommandWith took a second block")
	}
	env, err := hostEnv([]string{"A=1", "B=2"}, [][2]string{{"k", "v"}}, "A=3")
	if err != nil || strings.Join(env, " ") != "A=3 B=2 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=k GIT_CONFIG_VALUE_0=v" {
		t.Fatalf("hostEnv: %v %v", env, err)
	}
}
