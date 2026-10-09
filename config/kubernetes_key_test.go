package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// control_plane.kubernetes comes only from the managed file; a user's copy is
// set aside and named, with its values hidden since it may hold a kubeconfig.
func TestKubernetesDriverOptionsAreManagedOnly(t *testing.T) {
	if !ManagedOnly("control_plane.kubernetes") {
		t.Fatal("control_plane.kubernetes is not managed only")
	}
	user := `{"control_plane":{"enabled":true,"kubernetes":{"namespace":"evil","connection":{"kubeconfig":"{\"users\":[{\"user\":{\"token\":\"s3cr3t\"}}]}"}}}}`
	_, ws := trustHome(t, user, "")
	cfg, err := LoadWith(ws, LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ControlPlane.Kubernetes) != 0 {
		t.Fatalf("the user's file configured the Kubernetes driver: %s", cfg.ControlPlane.Kubernetes)
	}
	var aside *SetAsideKey
	for i := range cfg.SetAside {
		if cfg.SetAside[i].Key == "control_plane.kubernetes" {
			aside = &cfg.SetAside[i]
		}
	}
	if aside == nil {
		t.Fatalf("control_plane.kubernetes was not named as set aside: %+v", cfg.SetAside)
	}
	if s := aside.String(); strings.Contains(s, "s3cr3t") || strings.Contains(s, "evil") {
		t.Fatalf("the set-aside warning shows the options' values: %s", s)
	}

	withManaged(t, `{"control_plane":{"kubernetes":{"namespace":"abhed","connection":{"server":"https://kube.example"}}}}`)
	cfg, err = LoadWith(t.TempDir(), LoadOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(cfg.ControlPlane.Kubernetes, &got); err != nil || got["namespace"] != "abhed" {
		t.Fatalf("the managed options were not kept as written: %s (%v)", cfg.ControlPlane.Kubernetes, err)
	}
}

// CE checks only that the options are one JSON object; the edition decodes them.
func TestKubernetesDriverOptionsMustBeAnObject(t *testing.T) {
	for _, v := range []string{`"opts.json"`, `["a"]`, `3`, `null`, `true`} {
		withManaged(t, `{"control_plane":{"kubernetes":`+v+`}}`)
		_, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true})
		if err == nil || !strings.Contains(err.Error(), "control_plane.kubernetes must be a JSON object") {
			t.Errorf("%s: %v", v, err)
		}
	}
	withManaged(t, `{"control_plane":{"kubernetes":{"anything":{"the":"edition decides"}}}}`)
	if _, err := LoadWith(t.TempDir(), LoadOptions{Quiet: true}); err != nil {
		t.Errorf("an object with keys CE does not know was refused: %v", err)
	}
}
