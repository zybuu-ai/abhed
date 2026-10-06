package cgroup

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUnitOf(t *testing.T) {
	for _, c := range []struct {
		dir           string
		unit          string
		user, managed bool
	}{
		{"/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-u7.scope", "run-u7.scope", true, true},
		{"/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-u7.scope/sub", "run-u7.scope", true, true},
		{"/sys/fs/cgroup/user.slice/user-1000.slice/session-3.scope", "session-3.scope", false, true},
		{"/sys/fs/cgroup/system.slice/abhed.service", "abhed.service", false, true},
		{"/sys/fs/cgroup/user.slice/user-1000.slice", "", false, true},
		{"/sys/fs/cgroup/container", "", false, false},
	} {
		unit, user, managed := unitOf(c.dir)
		if unit != c.unit || user != c.user || managed != c.managed {
			t.Errorf("unitOf(%s) = %q %v %v, want %q %v %v", c.dir, unit, user, managed, c.unit, c.user, c.managed)
		}
	}
}

// A scope systemd still manages is refused even though this user may write it.
func TestConfirmDelegatedAsksTheManager(t *testing.T) {
	const scope = "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/run-u7.scope"
	owned := func(string) error { t.Fatal("ownership asked under systemd"); return nil }
	answer := func(v string, err error) Manager {
		return func(_ context.Context, unit string, user bool) (string, error) {
			if unit != "run-u7.scope" || !user {
				t.Fatalf("asked about %s (user %v)", unit, user)
			}
			return v, err
		}
	}
	if err := confirmDelegated(context.Background(), scope, answer("yes\n", nil), owned); err != nil {
		t.Fatalf("delegated scope refused: %v", err)
	}
	for _, c := range []struct {
		v    string
		err  error
		want string
	}{
		{"no\n", nil, "Delegate=no"},
		{"", nil, "Delegate=(nothing)"},
		{"", errors.New("no bus"), "could not be asked"},
	} {
		err := confirmDelegated(context.Background(), scope, answer(c.v, c.err), owned)
		if !errors.Is(err, ErrNotDelegated) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("answer %q %v: err = %v, want %q", c.v, c.err, err, c.want)
		}
	}
}

func TestConfirmDelegatedOutsideSystemd(t *testing.T) {
	never := func(context.Context, string, bool) (string, error) { t.Fatal("systemd asked"); return "", nil }
	called := false
	if err := confirmDelegated(context.Background(), "/sys/fs/cgroup/ctr", never, func(string) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("err %v, ownership checked %v", err, called)
	}
	err := confirmDelegated(context.Background(), "/sys/fs/cgroup/user.slice", never, func(string) error { return nil })
	if !errors.Is(err, ErrNotDelegated) {
		t.Fatalf("a slice with no unit: err = %v", err)
	}
}
