package controlplane_test

import (
	"errors"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	cp "github.com/zybuu-ai/abhed/controlplane"
)

func TestSettingsDefaultsAndValidation(t *testing.T) {
	st, err := cp.NewSettings(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := st.For(nil)
	if v[cp.SetRestartPolicy] != cp.RestartNever || v.Int(cp.SetExecTimeout) != 600 {
		t.Fatalf("defaults %v", v)
	}
	for _, bad := range []map[string]string{
		{cp.SetExecTimeout: "0"},
		{cp.SetRestartPolicy: "sometimes"},
		{cp.SetMinTier: "steel"},
		{"no.such": "1"},
	} {
		if _, err := cp.NewSettings(bad, nil); !errors.Is(err, cp.ErrInvalid) {
			t.Errorf("%v accepted: %v", bad, err)
		}
	}
}

// Managed values are a ceiling a sandbox may only tighten; without one a
// sandbox may set any valid value.
func TestManagedIsACeilingNotAnOverride(t *testing.T) {
	st, err := cp.NewSettings(map[string]string{cp.SetRestartPolicy: cp.RestartOnFailure, cp.SetExecOutput: "4096"},
		map[string]bool{cp.SetRestartPolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Check(map[string]string{cp.SetRestartPolicy: cp.RestartAlways}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("always under an on-failure ceiling: %v", err)
	}
	if err := st.Check(map[string]string{cp.SetRestartPolicy: cp.RestartNever}); err != nil {
		t.Fatalf("never under an on-failure ceiling: %v", err)
	}
	// Not managed: the configured value is a default a sandbox may change.
	if err := st.Check(map[string]string{cp.SetExecOutput: "8192"}); err != nil {
		t.Fatalf("a looser unmanaged value: %v", err)
	}
	// A ceiling that tightened after a sandbox was set still holds it.
	if got := st.For(map[string]string{cp.SetRestartPolicy: cp.RestartAlways})[cp.SetRestartPolicy]; got != cp.RestartOnFailure {
		t.Fatalf("effective policy %q above the ceiling", got)
	}
	if got := st.For(map[string]string{cp.SetRestartPolicy: cp.RestartNever})[cp.SetRestartPolicy]; got != cp.RestartNever {
		t.Fatalf("effective policy %q, want the sandbox's tighter one", got)
	}
}

func TestSettingsFromConfigReadsManagedKeys(t *testing.T) {
	cfg := config.Default()
	cfg.ControlPlane.ExecTimeoutSeconds = 30
	cfg.ControlPlane.MaxRestarts = new(int)
	cfg.ManagedKeys = []string{"control_plane.exec_timeout_seconds"}
	st, err := cp.SettingsFromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v := st.For(nil); v.Int(cp.SetExecTimeout) != 30 || v.Int(cp.SetMaxRestarts) != 0 {
		t.Fatalf("values %v", v)
	}
	if err := st.Check(map[string]string{cp.SetExecTimeout: "60"}); !errors.Is(err, cp.ErrInvalid) {
		t.Fatalf("looser than managed: %v", err)
	}
	found := false
	for _, info := range st.List() {
		if info.Key == cp.SetExecTimeout {
			found = info.Ceiling && info.Source == "managed"
		}
	}
	if !found {
		t.Fatalf("list does not show the managed ceiling: %+v", st.List())
	}
}

func TestTierMeets(t *testing.T) {
	for _, c := range []struct {
		tier, min string
		want      bool
	}{
		{"process", "", true}, {"process", "process", true}, {"fence", "process", true},
		{"process", "container", false}, {"vm", "container", true}, {"", "process", false}, {"weird", "none", false},
	} {
		if got := cp.TierMeets(c.tier, c.min); got != c.want {
			t.Errorf("TierMeets(%q, %q) = %v", c.tier, c.min, got)
		}
	}
}
