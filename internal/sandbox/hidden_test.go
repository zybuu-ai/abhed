package sandbox

import (
	"fmt"
	"path/filepath"
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
	allow := strings.Index(prof, fmt.Sprintf("(allow file-read* (subpath %q))", ws))
	state := strings.Index(prof, fmt.Sprintf("(deny file-read* (subpath %q))", filepath.Join(ws, stateDir)))
	if deny < 0 || allow < deny || state < allow {
		t.Fatalf("order deny %d, allow %d, state %d in\n%s", deny, allow, state, prof)
	}
	// Writes too, after the temp and cache allows that may hold the folder.
	wdeny := strings.LastIndex(prof, fmt.Sprintf("(deny file-write* (subpath %q))", root))
	wallow := strings.LastIndex(prof, fmt.Sprintf("(allow file-write* (subpath %q))", ws))
	temp := strings.LastIndex(prof, `(allow file-write* (subpath "/tmp"))`)
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
