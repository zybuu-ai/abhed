package app

import (
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
)

// The person's kept grants are listed and forgotten over ACP without a
// session, and never with a credential.
func TestACPMachineListAndForget(t *testing.T) {
	r := newStudioRig(t, "")
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindK8sLogin, Name: "lab", Identity: "i",
		What:  "cluster lab at https://lab.example:6443 with secret K8S_LAB_TOKEN",
		Login: &machinegrant.Login{Server: "https://lab.example:6443", CAPEM: "-----BEGIN CERTIFICATE-----x", Secret: "K8S_LAB_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Grants []struct{ Kind, Name, What string } `json:"grants"`
	}
	m := r.cl.call("_abhed/machine/list", map[string]any{})
	if m.Error != nil || strings.Contains(string(m.Result), "BEGIN CERTIFICATE") {
		t.Fatalf("list: %+v %s", m.Error, m.Result)
	}
	r.cl.ok("_abhed/machine/list", map[string]any{}, &list)
	if len(list.Grants) != 1 || list.Grants[0].Kind != "k8s-login" || list.Grants[0].Name != "lab" {
		t.Fatalf("list %+v", list)
	}
	r.cl.refused(errParams, "_abhed/machine/forget", map[string]any{"kind": "bogus", "name": "lab"})
	r.cl.refused(errParams, "_abhed/machine/forget", map[string]any{"kind": "ssh-host", "name": "lab"})
	r.cl.ok("_abhed/machine/forget", map[string]any{"kind": "k8s-login", "name": "lab"}, nil)
	if gs, _ := machinegrant.List(); len(gs) != 0 {
		t.Fatalf("still kept %v", gs)
	}
}
