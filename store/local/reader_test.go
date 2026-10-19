package local

import (
	"context"
	"os"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/store"
)

// A reader that keeps the End of the last line it read goes on from there,
// and an offset that is not a line's start reads from the beginning.
func TestLinesFromKeepsItsPlace(t *testing.T) {
	var appended []Sealed
	s, err := Open(Options{Dir: t.TempDir(), User: "tester", OnAppend: func(l Sealed) { appended = append(appended, l) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSession(context.Background(), store.SessionRecord{ID: "s-1", User: "tester", Workspace: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	r := agent.NewRecorder(s, "s-1", "")
	for i := 0; i < 5; i++ {
		if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	all, end, err := s.LinesFrom("s-1", 0)
	if err != nil || len(all) != 5 {
		t.Fatalf("%d lines, %v", len(all), err)
	}
	if size, _ := s.Size("s-1"); size != end {
		t.Fatalf("end %d, file %d", end, size)
	}
	for i := range all {
		if all[i].End != appended[i].End || all[i].Hash != appended[i].Hash {
			t.Fatalf("line %d: read %d/%s, appended %d/%s", i, all[i].End, all[i].Hash, appended[i].End, appended[i].Hash)
		}
	}
	rest, end2, err := s.LinesFrom("s-1", all[2].End)
	if err != nil || len(rest) != 2 || rest[0].Seq != all[3].Seq || end2 != end {
		t.Fatalf("from line 3: %d lines, end %d, %v", len(rest), end2, err)
	}
	if again, _, _ := s.LinesFrom("s-1", all[2].End+3); len(again) != 5 {
		t.Fatalf("an offset inside a line read %d lines, not all 5", len(again))
	}
	if none, at, _ := s.LinesFrom("s-1", end); len(none) != 0 || at != end {
		t.Fatalf("at the end: %d lines, %d", len(none), at)
	}
	// An unfinished last line is not read, and not counted.
	f, _ := os.OpenFile(s.Path("s-1"), os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.WriteString(`{"seq":9`)
	_ = f.Close()
	if more, at, _ := s.LinesFrom("s-1", end); len(more) != 0 || at != end {
		t.Fatalf("a torn line: %d lines, %d", len(more), at)
	}
}
