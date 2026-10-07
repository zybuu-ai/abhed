package mountns

import (
	"reflect"
	"testing"
)

// An ostree host (Fedora CoreOS, Silverblue) as its mountinfo shows it: /var
// and /sysroot are the same filesystem, so a workspace under /var/home is
// reachable through /sysroot too. A bind mount of a folder holding the
// workspace, and one of a folder inside it, are aliases as well; other
// filesystems and unrelated folders are not.
const ostreeMountinfo = `
1 0 0:30 / / ro,relatime - overlay composefs ro
60 1 252:4 / /sysroot ro,relatime - xfs /dev/vda4 rw
64 60 252:4 /ostree/deploy/fedora-coreos/var /sysroot/ostree/deploy/fedora-coreos/var rw - xfs /dev/vda4 rw
61 1 252:4 /ostree/deploy/fedora-coreos/deploy/abc.0/etc /etc rw,relatime - xfs /dev/vda4 rw
62 1 252:4 /ostree/deploy/fedora-coreos/var /var rw,relatime - xfs /dev/vda4 rw
63 62 0:40 / /var/tmp rw - tmpfs tmpfs rw
70 1 252:4 /ostree/deploy/fedora-coreos/var/home/core /mnt/my\040home rw - xfs /dev/vda4 rw
71 1 252:4 /ostree/deploy/fedora-coreos/var/home/core/ws/.git /mnt/git rw - xfs /dev/vda4 rw
72 1 252:4 /ostree/deploy/fedora-coreos/var/home/other /mnt/other rw - xfs /dev/vda4 rw
73 1 252:4 /ostree/deploy/fedora-coreos/var/home/core/ws/gone//deleted /mnt/gone rw - xfs /dev/vda4 rw
74 1 252:5 / /mnt/disk rw - xfs /dev/vda5 rw
`

func TestAliasesOnAnOstreeHost(t *testing.T) {
	entries, err := parseMountinfo(ostreeMountinfo)
	if err != nil {
		t.Fatal(err)
	}
	if entries[6].Point != "/mnt/my home" {
		t.Fatalf("the escaped space: %q", entries[6].Point)
	}
	found, deleted, err := aliases(entries, 62, "/var/home/core/ws")
	if err != nil {
		t.Fatal(err)
	}
	want := []alias{
		{Path: "/sysroot/ostree/deploy/fedora-coreos/var/home/core/ws"},
		{Path: "/mnt/my home/ws"},
		{Path: "/mnt/git", Rel: ".git"},
	}
	if !reflect.DeepEqual(found, want) {
		t.Errorf("aliases:\n got %+v\nwant %+v", found, want)
	}
	if !reflect.DeepEqual(deleted, []string{"/mnt/gone"}) {
		t.Errorf("deleted: %v", deleted)
	}
	if _, _, err := aliases(entries, 99, "/var/home/core/ws"); err == nil {
		t.Error("a missing mount was not refused")
	}
	if _, _, err := aliases(entries, 63, "/var/home/core/ws"); err == nil {
		t.Error("a workspace outside its mount was not refused")
	}
	// A workspace on a filesystem mounted once has none.
	if found, deleted, err := aliases(entries, 74, "/mnt/disk/ws"); err != nil || len(found)+len(deleted) != 0 {
		t.Errorf("a single mount: %v %v %v", found, deleted, err)
	}
}

// The part of a plan inside an alias that shows only part of the workspace:
// paths below it are moved under it, and one it lies in covers it whole.
func TestRebase(t *testing.T) {
	p := Plan{Root: "/w", Pin: []string{".git"}, ReadOnly: []string{".git/config", ".git/hooks", ".vscode/settings.json"}, Empty: []string{".abhed", "sub/.abhed"}}
	for _, c := range []struct {
		rel   string
		sub   Plan
		whole string
	}{
		{".git", Plan{ReadOnly: []string{"config", "hooks"}}, ""},
		{".git/hooks", Plan{}, wholeReadOnly},
		{".git/hooks/pre-commit", Plan{}, wholeReadOnly},
		{".abhed", Plan{}, wholeEmpty},
		{".abhed/users.json", Plan{}, wholeEmpty},
		{"sub", Plan{Empty: []string{".abhed"}}, ""},
		{"src", Plan{}, ""},
		{".gitx", Plan{}, ""},
	} {
		sub, whole := rebase(p, c.rel)
		if whole != c.whole || !reflect.DeepEqual(sub, c.sub) {
			t.Errorf("%s: got %+v %q, want %+v %q", c.rel, sub, whole, c.sub, c.whole)
		}
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		parent, child, rest string
		ok                  bool
	}{
		{"/", "/a/b", "a/b", true},
		{"/a", "/a", "", true},
		{"/a", "/a/b", "b", true},
		{"/a", "/ab", "", false},
		{"/a/b", "/a", "", false},
	} {
		if rest, ok := within(c.parent, c.child); rest != c.rest || ok != c.ok {
			t.Errorf("within(%q, %q) = %q %v", c.parent, c.child, rest, ok)
		}
	}
}
