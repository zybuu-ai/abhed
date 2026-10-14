//go:build !windows

package guest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func collect(t *testing.T, s *Server, fail error) ([]string, error) {
	t.Helper()
	var got []string
	err := SyncWith(context.Background(), pair(t, s), func(c Change) error {
		if fail != nil {
			return fail
		}
		switch {
		case c.Deleted:
			got = append(got, "-"+c.Path)
		default:
			b, _ := io.ReadAll(c.Body)
			got = append(got, string(c.Header.Typeflag)+c.Path+"="+string(b)+c.Header.Linkname)
		}
		return nil
	})
	sort.Strings(got)
	return got, err
}

func TestSyncSendsOnlyChanges(t *testing.T) {
	s := server(t)
	ws := s.Dir
	for _, f := range []string{"keep", "edit", "gone"} {
		if err := os.WriteFile(filepath.Join(ws, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := NewTracker(ws)
	if err != nil {
		t.Fatal(err)
	}
	s.Workspace = tr
	time.Sleep(2 * slack)
	if got, err := collect(t, s, nil); err != nil || len(got) != 0 {
		t.Fatalf("an unchanged workspace sent %q %v", got, err)
	}
	time.Sleep(20 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(ws, "edit"), []byte("edited"), 0o644)
	_ = os.Remove(filepath.Join(ws, "gone"))
	_ = os.MkdirAll(filepath.Join(ws, "dir", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, "dir", "sub", "new"), []byte("new"), 0o644)
	_ = os.Symlink("keep", filepath.Join(ws, "link"))
	time.Sleep(2 * slack)
	got, err := collect(t, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-gone", "0dir/sub/new=new", "0edit=edited", "2link=keep", "5dir/sub=", "5dir="}
	if len(got) != len(want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q\nwant %q", got, want)
		}
	}
	// Synced once, they are not sent again. (A change within the clock's
	// slack of a sync is sent twice, which the sleep below avoids.)
	if got, err := collect(t, s, nil); err != nil || len(got) != 0 {
		t.Fatalf("a second sync sent %q %v", got, err)
	}
}

func TestSyncWithoutAckIsSentAgain(t *testing.T) {
	s := server(t)
	tr, err := NewTracker(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Workspace = tr
	_ = os.WriteFile(filepath.Join(s.Dir, "f"), []byte("x"), 0o644)
	if _, err := collect(t, s, errors.New("host refused")); err == nil {
		t.Fatal("a failed apply was acknowledged")
	}
	got, err := collect(t, s, nil)
	if err != nil || len(got) != 1 || got[0] != "0f=x" {
		t.Fatalf("the change was not sent again: %q %v", got, err)
	}
}

func TestSyncWithoutWorkspaceIsAnError(t *testing.T) {
	if _, err := collect(t, server(t), nil); err == nil {
		t.Fatal("a guest without a workspace synced")
	}
}
