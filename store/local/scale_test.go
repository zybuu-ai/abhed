package local

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/store"
)

// With 2,000 sessions in the index, creating one more stays within 5 ms
// (median), and looking one up does not re-read the whole index each time.
func TestIndexScales(t *testing.T) {
	if testing.Short() || raceEnabled || os.Getenv("ABHED_SKIP_BUDGETS") != "" {
		t.Skip("budgets are measured without -short and -race")
	}
	s := openTest(t, t.TempDir())
	for i := range 2000 {
		if err := s.CreateSession(context.Background(), store.SessionRecord{ID: fmt.Sprintf("s-%d", i), Workspace: "/w"}); err != nil {
			t.Fatal(err)
		}
		_ = s.Release(fmt.Sprintf("s-%d", i))
	}
	var d []time.Duration
	for i := range 21 {
		start := time.Now()
		if err := s.CreateSession(context.Background(), store.SessionRecord{ID: fmt.Sprintf("n-%d", i), Workspace: "/w"}); err != nil {
			t.Fatal(err)
		}
		d = append(d, time.Since(start))
		_ = s.Release(fmt.Sprintf("n-%d", i))
	}
	slices.Sort(d)
	if d[10] > 5*time.Millisecond {
		t.Errorf("CreateSession at 2,000 sessions took %v (median), budget 5ms", d[10])
	}
	start := time.Now()
	for range 20 {
		if _, err := s.GetSession(context.Background(), "s-1000"); err != nil {
			t.Fatal(err)
		}
	}
	if per := time.Since(start) / 20; per > 2*time.Millisecond {
		t.Errorf("GetSession at 2,000 sessions took %v, budget 2ms", per)
	}
}
