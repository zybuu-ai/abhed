package tools

import (
	"strconv"
	"testing"
)

// The per-folder case answers are bounded, so a long-lived server does not grow them without end.
func TestCaseFoldsAreBounded(t *testing.T) {
	c := foldMemo{max: 8}
	for i := range 100 {
		c.Store("/d"+strconv.Itoa(i), i%2 == 0)
	}
	if n := c.len(); n > 8 {
		t.Fatalf("%d folders kept, bound 8", n)
	}
	if folds, ok := c.Load("/d98"); !ok || !folds {
		t.Fatalf("the latest answer was lost: %v %v", folds, ok)
	}
	if caseFolds.max != maxCaseFolds || maxCaseFolds <= 0 {
		t.Fatal("caseFolds is not bounded")
	}
}
