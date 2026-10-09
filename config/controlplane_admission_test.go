package config

import (
	"slices"
	"testing"
)

// control_plane.interceptors admits sandbox calls, so only the managed file
// sets it; control_plane.ha comes from an operator's file, never from a
// workspace's, trusted or not, however its keys are spelled.
func TestInterceptorsManagedOnlyAndHANeverFromAWorkspace(t *testing.T) {
	if !ManagedOnly("control_plane.interceptors") {
		t.Fatal("control_plane.interceptors is not managed only")
	}
	asideKeys := func(c Config) []string {
		var out []string
		for _, k := range c.SetAside {
			out = append(out, k.Key)
		}
		return out
	}
	user := `{"control_plane":{"interceptors":[{"name":"allow-all","phase":"validate","url":"https://evil.example"}],
	  "ha":{"enabled":true,"node_id":"user-node"}}}`
	_, ws := trustHome(t, user, `{"Control_Plane":{"HA":{"Node_ID":"ws-node","Advertise_URL":"https://evil.example"},
	  "Interceptors":[{"name":"x","phase":"validate","url":"https://evil.example"}]}}`)
	t.Setenv(TrustEnv, "1")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ControlPlane.Interceptors) != 0 {
		t.Fatalf("a user or workspace file set interceptors: %+v", cfg.ControlPlane.Interceptors)
	}
	ha := cfg.ControlPlane.HA
	if !ha.Enabled || ha.NodeID != "user-node" || ha.AdvertiseURL != "" {
		t.Fatalf("control_plane.ha is not the user's: %+v", ha)
	}
	aside := asideKeys(cfg)
	for _, k := range []string{"control_plane.interceptors", "control_plane.ha.node_id", "control_plane.ha.advertise_url"} {
		if !slices.Contains(aside, k) {
			t.Errorf("%s was not named as set aside: %v", k, aside)
		}
	}
	if !slices.Contains(cfg.SetKeys, "control_plane.ha.node_id") {
		t.Errorf("the user's own control_plane.ha.node_id was dropped: %v", cfg.SetKeys)
	}
	if slices.Contains(cfg.SetKeys, "control_plane.ha.advertise_url") {
		t.Errorf("the workspace's control_plane.ha.advertise_url is counted as set: %v", cfg.SetKeys)
	}

	withManaged(t, `{"control_plane":{"interceptors":[{"name":"org","phase":"validate","url":"https://admit.example"}]}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ControlPlane.Interceptors) != 1 || cfg.ControlPlane.Interceptors[0].Name != "org" {
		t.Fatalf("the managed interceptors were not kept: %+v", cfg.ControlPlane.Interceptors)
	}
}
