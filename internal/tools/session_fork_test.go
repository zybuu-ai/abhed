package tools

import "testing"

// A fork shares the boundary and the checkpoint, and nothing that moves.
func TestForkKeepsTheBoundaryAndItsOwnDirectory(t *testing.T) {
	s, err := NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var checkpointed int
	s.Checkpoint = func(string, []byte, bool) { checkpointed++ }
	f := s.Fork()

	f.Cwd = f.Root + "/sub"
	f.MarkRead(f.Root+"/a", "x")
	if s.Cwd != s.Root || s.WasRead(s.Root+"/a") {
		t.Fatal("a fork moved or marked the session it came from")
	}
	if _, err := f.Resolve("/etc/passwd"); err == nil {
		t.Fatal("a fork lost the workspace boundary")
	}
	f.recordChange(f.Root + "/a")
	if checkpointed != 1 {
		t.Fatal("a change through a fork was not checkpointed")
	}
}
