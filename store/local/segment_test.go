package local

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rawLines(t *testing.T, s *Store, id string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(s.Path(id))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
}

func TestCheckSegmentFollowsTheChainAcrossBatches(t *testing.T) {
	s := openTest(t, t.TempDir())
	record(t, s, "s-1", "one", "two", "three", "four")
	raws := rawLines(t, s, "s-1")
	first, err := CheckSegment(raws[:2], "s-1", Genesis)
	if err != nil || len(first) != 2 {
		t.Fatalf("first batch: %v", err)
	}
	rest, err := CheckSegment(raws[2:], "s-1", first[1].Hash)
	if err != nil || len(rest) != len(raws)-2 || rest[0].Prev != first[1].Hash || !bytes.Equal(rest[0].Raw, raws[2]) {
		t.Fatalf("second batch: %v", err)
	}
	for name, c := range map[string]struct {
		raws    [][]byte
		session string
		before  string
		why     string
	}{
		"wrong before":  {raws[2:], "s-1", Genesis, "does not follow"},
		"gap":           {[][]byte{raws[0], raws[2]}, "s-1", Genesis, "does not follow"},
		"other session": {raws[:1], "s-2", Genesis, "belongs to session"},
		"changed":       {[][]byte{bytes.Replace(raws[0], []byte("one"), []byte("uno"), 1)}, "s-1", Genesis, "hash does not match"},
		"bad before":    {raws[:1], "s-1", "nope", "not a sha256"},
		"bad session":   {raws[:1], "../x", Genesis, "cannot name"},
	} {
		if _, err := CheckSegment(c.raws, c.session, c.before); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWriteExportVerifiesAsAnExport(t *testing.T) {
	s := openTest(t, t.TempDir())
	record(t, s, "s-1", "one", "two")
	raws := rawLines(t, s, "s-1")
	path := filepath.Join(t.TempDir(), "s-1.jsonl")
	var b bytes.Buffer
	head, err := WriteExport(&b, "s-1", raws)
	if err != nil || head.Lines != int64(len(raws)) {
		t.Fatalf("export: %v %+v", err, head)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := VerifyFile(path)
	if err != nil || !rep.OK || rep.Head != head {
		t.Fatalf("the export does not verify: %v %+v", err, rep)
	}
	// Lines that do not chain are exported marked so, and do not verify.
	b.Reset()
	if _, err := WriteExport(&b, "s-1", [][]byte{raws[1]}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(path, b.Bytes(), 0o600)
	if rep, _ := VerifyFile(path); rep.OK {
		t.Fatal("a broken chain exported as verified")
	}
}
