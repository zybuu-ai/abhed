package app

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/machinegrant"
)

func TestMachineListAndForget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "machine-grants.json")
	old := machinegrant.Path
	machinegrant.Path = func() string { return p }
	t.Cleanup(func() { machinegrant.Path = old })
	var out, errw bytes.Buffer
	if code := machineCmd([]string{"list"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "nothing") {
		t.Fatalf("empty list: %d %q %q", code, out.String(), errw.String())
	}
	if err := machinegrant.Keep(machinegrant.Grant{Kind: machinegrant.KindSSHHost, Name: "box", Identity: "i", What: "host box (me@10.0.0.9:22)"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := machineCmd([]string{"list"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "host box") {
		t.Fatalf("list: %d %q", code, out.String())
	}
	if code := machineCmd([]string{"forget", "ssh-host", "nope"}, &out, &errw); code != 1 {
		t.Fatalf("forgetting what is not kept: %d", code)
	}
	if code := machineCmd([]string{"forget", "bogus", "box"}, &out, &errw); code != 2 {
		t.Fatalf("a bad kind: %d", code)
	}
	out.Reset()
	if code := machineCmd([]string{"forget", "ssh-host", "box"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "asks again") {
		t.Fatalf("forget: %d %q", code, out.String())
	}
	if gs, _ := machinegrant.List(); len(gs) != 0 {
		t.Fatalf("still kept: %v", gs)
	}
}
