package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/controlplane"
	"github.com/zybuu-ai/abhed/store/chain"
)

// AuditLinks and AuditRows give each row's place in the chain as the chain
// holds it: the last one's hash is the head.
func TestAuditLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), filepath.Join(dir, "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		if err := s.Append(context.Background(), controlplane.AuditEvent{At: time.Now(), Principal: "alice", Action: "create", SandboxID: "sb", Outcome: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	var seqs []int64
	var last chain.Link
	if err := s.AuditRows(context.Background(), func(a controlplane.AuditEvent, l chain.Link) error {
		seqs, last = append(seqs, a.Seq), l
		return nil
	}); err != nil || len(seqs) != 3 {
		t.Fatalf("%d rows, %v", len(seqs), err)
	}
	head, _ := s.AuditChain().Head(context.Background(), "")
	if last.Pos != 3 || last.Hash != head.Hash {
		t.Fatalf("last %+v, head %+v", last, head)
	}
	links, err := s.AuditLinks(context.Background(), seqs[1:])
	if err != nil || len(links) != 2 || links[seqs[2]].Hash != head.Hash || links[seqs[1]].Pos != 2 {
		t.Fatalf("%v %v", links, err)
	}
}
