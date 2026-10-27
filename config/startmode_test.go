package config

import "testing"

// A session starts in permissions.mode where one was chosen, else in
// mode.default, which is never bypass, else in default.
func TestStartMode(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{Permissions: PermissionsConfig{Mode: "default"}}, "default"},
		{Config{Permissions: PermissionsConfig{Mode: "default"}, Mode: ModeConfig{Default: "auto"}}, "auto"},
		{Config{Permissions: PermissionsConfig{Mode: "plan"}, Mode: ModeConfig{Default: "auto"}}, "plan"},
		{Config{Permissions: PermissionsConfig{Mode: "default"}, Mode: ModeConfig{Default: "bypass"}}, "default"},
		{Config{Permissions: PermissionsConfig{Mode: "default"}, Mode: ModeConfig{Default: "auto"}, SetKeys: []string{"permissions.mode"}}, "default"},
		{Config{Permissions: PermissionsConfig{Mode: "default"}, Mode: ModeConfig{Default: "auto"}, modeChosen: true}, "default"},
	} {
		if got := c.cfg.StartMode(); got != c.want {
			t.Errorf("%+v: %s, want %s", c.cfg.Mode, got, c.want)
		}
	}
	c := Default()
	c.Mode.Default = "bypass"
	if err := c.Validate(); err == nil {
		t.Error("mode.default bypass was accepted")
	}
}
