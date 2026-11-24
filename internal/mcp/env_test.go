package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stdio server gets PATH and the other base names, what its env lists, and
// nothing else of Abhed's environment: no provider key, no secret.
func TestStdioServerGetsAMinimalEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-canary-provider")
	t.Setenv("ABHED_DATABASE_URL", "postgres://canary")
	t.Setenv("GITHUB_TOKEN", "gh-canary-listed")
	t.Setenv("PATH", "/usr/bin:/bin")
	dump := func(env []string) string {
		out := filepath.Join(t.TempDir(), "env")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g := NewGateway()
		defer g.Close()
		_ = g.Connect(ctx, []ServerConfig{{Name: "envdump", Command: "/bin/sh", Args: []string{"-c", "env > " + out + ".tmp; mv " + out + ".tmp " + out},
			Env: env, Enabled: true}})
		var data []byte
		for i := 0; i < 100 && len(data) == 0; i++ {
			data, _ = os.ReadFile(out)
			time.Sleep(20 * time.Millisecond)
		}
		return string(data)
	}
	for _, c := range []struct {
		env  []string
		want []string
	}{
		{nil, []string{"PATH=/usr/bin:/bin\n"}},
		{[]string{"GITHUB_TOKEN", "MODE=ro"}, []string{"PATH=/usr/bin:/bin\n", "GITHUB_TOKEN=gh-canary-listed\n", "MODE=ro\n"}},
	} {
		env := dump(c.env)
		for _, want := range c.want {
			if !strings.Contains(env, want) {
				t.Errorf("env %q: missing %q in:\n%s", c.env, want, env)
			}
		}
		leaks := []string{"sk-canary-provider", "postgres://canary"}
		if c.env == nil {
			leaks = append(leaks, "gh-canary-listed")
		}
		for _, leak := range leaks {
			if strings.Contains(env, leak) {
				t.Errorf("env %q: %q reached the server:\n%s", c.env, leak, env)
			}
		}
	}
}

func TestServerEnvConfiguredWins(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	env := ServerEnv([]string{"HOME=/srv", "UNSET_NAME_X"})
	if env[len(env)-1] != "HOME=/srv" || strings.Contains(strings.Join(env, "\n"), "UNSET_NAME_X") {
		t.Fatalf("env: %q", env)
	}
}

// Who is working reaches a server from Abhed's environment only: a server's
// configured env can neither set nor clear ABHED_GUEST or ABHED_ACCOUNT.
func TestServerEnvCarriesWhoIsWorking(t *testing.T) {
	has := func(env []string, kv string) bool {
		for _, e := range env {
			if e == kv {
				return true
			}
		}
		return false
	}
	named := func(env []string, k string) int {
		n := 0
		for _, e := range env {
			if strings.HasPrefix(strings.ToUpper(e), k+"=") {
				n++
			}
		}
		return n
	}
	t.Setenv("ABHED_GUEST", "1")
	t.Setenv("ABHED_ACCOUNT", "")
	os.Unsetenv("ABHED_ACCOUNT")
	env := ServerEnv([]string{"ABHED_GUEST=0", "abhed_guest=", "ABHED_ACCOUNT=bob", " ABHED_ACCOUNT =bob", "MODE=ro"})
	if !has(env, "ABHED_GUEST=1") || named(env, "ABHED_GUEST") != 1 {
		t.Errorf("a guest's server must see ABHED_GUEST=1 and nothing else: %q", env)
	}
	if named(env, "ABHED_ACCOUNT") != 0 || named(env, " ABHED_ACCOUNT ") != 0 {
		t.Errorf("configured ABHED_ACCOUNT reached the server: %q", env)
	}
	if !has(env, "MODE=ro") {
		t.Errorf("an ordinary entry was dropped: %q", env)
	}

	os.Unsetenv("ABHED_GUEST")
	t.Setenv("ABHED_ACCOUNT", "alice")
	env = ServerEnv([]string{"ABHED_GUEST=1", "ABHED_ACCOUNT"})
	if !has(env, "ABHED_ACCOUNT=alice") || named(env, "ABHED_ACCOUNT") != 1 || named(env, "ABHED_GUEST") != 0 {
		t.Errorf("an account's server must see only ABHED_ACCOUNT=alice: %q", env)
	}
}
