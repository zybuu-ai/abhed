package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The other sandboxes' folder is denied before the workspace's own read is
// allowed, and the workspace's .abhed is denied after it, so it stays denied.
func TestSeatbeltHidesSiblingFolders(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "sbx_a")
	prof := NewProcess(Policy{Workspace: ws, Hidden: []string{root}}).seatbeltProfile()
	deny := strings.Index(prof, fmt.Sprintf("(deny file-read* (subpath %q))", root))
	// The last allow: home's own section may allow the workspace earlier.
	allow := strings.LastIndex(prof, fmt.Sprintf("(allow file-read* (subpath %q))", ws))
	state := strings.Index(prof, fmt.Sprintf("(deny file-read* (subpath %q))", filepath.Join(ws, stateDir)))
	if deny < 0 || allow < deny || state < allow {
		t.Fatalf("order deny %d, allow %d, state %d in\n%s", deny, allow, state, prof)
	}
	// Writes too, after the temp and cache allows that may hold the folder.
	wdeny := strings.LastIndex(prof, fmt.Sprintf("(deny file-write* (subpath %q))", root))
	wallow := strings.LastIndex(prof, fmt.Sprintf("(allow file-write* (subpath %q))", ws))
	temp := strings.LastIndex(prof, `var/tmp"))`)
	wstate := strings.Index(prof, fmt.Sprintf("(deny file-write* (subpath %q))", filepath.Join(ws, stateDir)))
	if temp < 0 || wdeny < temp || wallow < wdeny || wstate < wallow {
		t.Fatalf("write order temp %d, deny %d, allow %d, state %d in\n%s", temp, wdeny, wallow, wstate, prof)
	}
	if strings.Contains(NewProcess(Policy{Workspace: ws}).seatbeltProfile(), "Other sandboxes") {
		t.Fatal("a policy hiding nothing hides something")
	}
}

func TestSelectRefusesHiddenThatIsShown(t *testing.T) {
	root := t.TempDir()
	for _, p := range []Policy{
		{Workspace: filepath.Join(root, "a"), Hidden: []string{root}, ReadOnlyPaths: []string{filepath.Dir(root)}},
		{Workspace: filepath.Join(root, "a"), Hidden: []string{root}, ReadOnlyPaths: []string{filepath.Join(root, "b")}},
		{Workspace: filepath.Join(root, "a"), Hidden: []string{root}, SkillDirs: []string{filepath.Join(root, "b")}},
	} {
		if _, err := Select(p); err == nil || !strings.Contains(err.Error(), "sandbox") {
			t.Fatalf("selected %+v: %v", p, err)
		}
	}
}

// Under PrivateTemp the deny on the siblings' folder comes after the temp
// rules' allows, so a read-only path or a readable file among them stays hidden.
func TestHiddenWinsOverPrivateTempAllows(t *testing.T) {
	root := workspace(t)
	ws, sib := filepath.Join(root, "sbx_a"), filepath.Join(root, "sbx_b")
	for _, d := range []string{ws, sib} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sib, "secret.txt"), []byte("sibling-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := DefaultPolicy(ws)
	p.Hidden = []string{root}
	p.PrivateTemp = true
	p.ReadOnlyPaths = []string{sib}
	p.ReadableFiles = []ReadableFile{pinned(t, filepath.Join(sib, "status.sh"), "#!/bin/sh\necho status-ok\n")}
	s := NewProcess(p)
	t.Cleanup(func() { _ = s.removeTemp() })
	prof := s.seatbeltProfileFor(0, filepath.Join(sib, "ca"))
	deny := strings.LastIndex(prof, fmt.Sprintf("(deny file-read* (subpath %q))", root))
	for _, allow := range []string{
		fmt.Sprintf("(allow file-read* (subpath %q))", sib),
		fmt.Sprintf("(allow file-read* (subpath %q))", filepath.Join(sib, "ca")),
	} {
		if at := strings.LastIndex(prof, allow); at < 0 || at > deny {
			t.Errorf("%s at %d, after the siblings' deny at %d", allow, at, deny)
		}
	}
	if strings.Contains(prof, fmt.Sprintf("(allow file-read* (literal %q))", filepath.Join(sib, "status.sh"))) {
		t.Error("a readable file among the siblings' folders is allowed")
	}
	if wdeny, wallow := strings.LastIndex(prof, fmt.Sprintf("(deny file-write* (subpath %q))", root)),
		strings.LastIndex(prof, fmt.Sprintf("(allow file-write* (subpath %q))", ws)); wdeny < 0 || wallow < wdeny {
		t.Errorf("the workspace's write allow at %d is not after the siblings' deny at %d", wallow, wdeny)
	}
	if runtime.GOOS != "darwin" {
		return
	}
	if ok, why := s.Available(); !ok {
		t.Skipf("process sandbox unavailable: %s", why)
	}
	out, _ := runIn(t, s, ws, `cat `+filepath.Join(sib, "secret.txt")+` 2>&1; echo x > own.txt && echo x > "$TMPDIR/own.txt" && echo wrote`)
	if strings.Contains(out, "sibling-secret") || !strings.Contains(out, "wrote") {
		t.Fatalf("%s", out)
	}
}
