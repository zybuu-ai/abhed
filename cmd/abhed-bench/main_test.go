package main

import "testing"

// A stack with caching off that still reports the field must get the same
// warning as one that reports nothing.
func TestCacheNote(t *testing.T) {
	for _, tc := range []struct {
		r    report
		want string
	}{
		{report{}, "did not report a cached-token figure"},
		{report{CacheReported: true}, "reported zero cached tokens on every turn"},
		{report{CacheReported: true, Summary: summaryStats{TotalCached: 900}}, ""},
	} {
		if got := cacheNote(tc.r); got != tc.want {
			t.Errorf("cacheNote(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}
