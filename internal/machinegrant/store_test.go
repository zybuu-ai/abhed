package machinegrant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTemp points the store at a file in a throwaway folder.
func useTemp(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state", "machine-grants.json")
	old := Path
	Path = func() string { return p }
	t.Cleanup(func() { Path = old })
	return p
}

func TestKeepKeptForget(t *testing.T) {
	p := useTemp(t)
	id := Identity("prod", "https://api.prod:6443")
	if Kept(KindK8sContext, "prod", id) {
		t.Fatal("kept before any grant")
	}
	if err := Keep(Grant{Kind: KindK8sContext, Name: "prod", Identity: id, What: "kubeconfig context prod"}); err != nil {
		t.Fatal(err)
	}
	if !Kept(KindK8sContext, "prod", id) {
		t.Fatal("not kept after Keep")
	}
	// Another server under the same name is another cluster: asked again.
	if Kept(KindK8sContext, "prod", Identity("prod", "https://evil:6443")) {
		t.Fatal("a grant covered a context whose server changed")
	}
	if Kept(KindSSHHost, "prod", id) {
		t.Fatal("a grant of one kind covered another")
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the file is %v, %v; want mode 0600", fi, err)
	}
	// Keeping again replaces, never duplicates.
	if err := Keep(Grant{Kind: KindK8sContext, Name: "prod", Identity: Identity("prod", "x")}); err != nil {
		t.Fatal(err)
	}
	if gs, _ := List(); len(gs) != 1 {
		t.Fatalf("%d grants after keeping one name twice", len(gs))
	}
	found, err := Forget(KindK8sContext, "prod")
	if err != nil || !found {
		t.Fatalf("Forget: %v %v", found, err)
	}
	if gs, _ := List(); len(gs) != 0 {
		t.Fatalf("%d grants after Forget", len(gs))
	}
	if found, _ := Forget(KindK8sContext, "prod"); found {
		t.Fatal("forgot a grant twice")
	}
}

func TestUnreadableFileGrantsNothingAndIsSetAside(t *testing.T) {
	p := useTemp(t)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(`{"grants":[{"kind":"ssh-host","name":"a","identity":"x"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if Kept(KindSSHHost, "a", "x") {
		t.Fatal("a truncated file granted something")
	}
	if err := Keep(Grant{Kind: KindSSHHost, Name: "b", Identity: "y"}); err != nil {
		t.Fatal(err)
	}
	aside, _ := filepath.Glob(p + ".unreadable-*")
	if len(aside) != 1 {
		t.Fatalf("the unreadable file was not set aside: %v", aside)
	}
	if !Kept(KindSSHHost, "b", "y") {
		t.Fatal("the new grant was not kept")
	}
}

func TestLinkedFileIsRefused(t *testing.T) {
	p := useTemp(t)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	other := filepath.Join(t.TempDir(), "other.json")
	if err := os.WriteFile(other, []byte(`{"grants":[{"kind":"ssh-host","name":"a","identity":"x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, p); err != nil {
		t.Skip("no hard links here")
	}
	if Kept(KindSSHHost, "a", "x") {
		t.Fatal("a hard-linked file granted something")
	}
	if _, err := List(); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("List of a linked file: %v", err)
	}
}

func TestLoginsAreTheKeptLogins(t *testing.T) {
	useTemp(t)
	if err := Keep(Grant{Kind: KindK8sLogin, Name: "lab", Identity: "i",
		Login: &Login{Server: "https://api.lab:6443", Secret: "K8S_LAB_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	_ = Keep(Grant{Kind: KindK8sContext, Name: "ctx", Identity: "j"})
	got := Logins()
	if len(got) != 1 || got["lab"].Secret != "K8S_LAB_TOKEN" {
		t.Fatalf("Logins = %+v", got)
	}
}

func TestIdentityIsUnambiguous(t *testing.T) {
	if Identity("ab", "c") == Identity("a", "bc") {
		t.Fatal("parts run together")
	}
}
