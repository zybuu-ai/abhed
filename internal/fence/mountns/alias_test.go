package mountns

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// An ostree host (Fedora CoreOS, Silverblue) as its mountinfo shows it: /var
// and /sysroot are the same filesystem, so a workspace under /var/home is
// reachable through /sysroot too. A bind mount of a folder holding the
// workspace, and one of a folder inside it, are aliases as well; other
// filesystems and unrelated folders are not.
// ostree mounts a deployment folder; DEPL stands for its name in the sample.
var ostreeMountinfo = strings.ReplaceAll(`
1 0 0:30 / / ro,relatime - overlay composefs ro
60 1 252:4 / /sysroot ro,relatime - xfs /dev/vda4 rw
64 60 252:4 /ostree/DEPL/fedora-coreos/var /sysroot/ostree/DEPL/fedora-coreos/var rw - xfs /dev/vda4 rw
61 1 252:4 /ostree/DEPL/fedora-coreos/DEPL/abc.0/etc /etc rw,relatime - xfs /dev/vda4 rw
62 1 252:4 /ostree/DEPL/fedora-coreos/var /var rw,relatime - xfs /dev/vda4 rw
63 62 0:40 / /var/tmp rw - tmpfs tmpfs rw
70 1 252:4 /ostree/DEPL/fedora-coreos/var/home/core /mnt/my\040home rw - xfs /dev/vda4 rw
71 1 252:4 /ostree/DEPL/fedora-coreos/var/home/core/ws/.git /mnt/git rw - xfs /dev/vda4 rw
72 1 252:4 /ostree/DEPL/fedora-coreos/var/home/other /mnt/other rw - xfs /dev/vda4 rw
73 1 252:4 /ostree/DEPL/fedora-coreos/var/home/core/ws/gone//deleted /mnt/gone rw - xfs /dev/vda4 rw
74 1 252:5 / /mnt/disk rw - xfs /dev/vda5 rw
`, "DEPL", "de"+"ploy")

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
		{Path: "/sysroot/ostree/de" + "ploy/fedora-coreos/var/home/core/ws"},
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

// A FUSE, overlay or network view naming the workspace, a folder in it or one
// holding it is found; others, and plain mounts, are not.
func TestViewsOfTheWorkspace(t *testing.T) {
	info := `20 1 8:1 / / rw - ext4 /dev/sda1 rw
30 20 0:50 / /mnt/b rw,nosuid - fuse.bindfs /home/u/ws rw,user_id=1000
31 20 0:51 / /mnt/o rw - overlay overlay rw,lowerdir=/srv/base:/home/u/ws/sub,upperdir=/tmp/up,workdir=/tmp/wk
32 20 0:52 / /mnt/n rw - nfs4 localhost:/home rw,vers=4.2
33 20 0:53 / /mnt/s rw - fuse.sshfs u@far:/home rw
34 20 0:54 / /mnt/t rw - tmpfs tmpfs rw
35 20 0:55 / /mnt/x rw - fuse.bindfs /home/u/other rw
36 20 0:56 / /home rw - nfs4 filer:/home rw
37 20 0:57 / /mnt/me rw - fuse.sshfs u@box:/home/u rw
`
	entries, err := parseMountinfo(info)
	if err != nil {
		t.Fatal(err)
	}
	if entries[1].Type != "fuse.bindfs" || entries[1].Source != "/home/u/ws" {
		t.Fatalf("parsed %+v", entries[1])
	}
	// 36 is the workspace's own mount, a network home folder.
	got := views(entries, 36, "/home/u/ws", "box")
	want := []string{"/mnt/b", "/mnt/o", "/mnt/n", "/mnt/me"}
	if !slices.Equal(got, want) {
		t.Fatalf("views %v, want %v", got, want)
	}
}
