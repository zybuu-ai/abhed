package app

import (
	"strings"
	"testing"
)

func TestEditionErrorArticle(t *testing.T) {
	for feature, want := range map[string]string{
		"telemetry":  "is an Enterprise feature",
		"kubernetes": "is a Team feature",
		"nosuch":     "is a paid feature",
	} {
		got := (&EditionError{Key: "k", Feature: feature, Edition: "Community"}).Error()
		if !strings.Contains(got, want) {
			t.Errorf("%s: %q lacks %q", feature, got, want)
		}
	}
}

// A later WithCommand for the same name drops the earlier description, which
// belonged to the command it replaced.
func TestWithCommandDropsTheOldAbout(t *testing.T) {
	first := WithCommandAbout("tally", "count the old way", func(string, []string) int { return 0 })
	second := WithCommand("tally", func(string, []string) int { return 0 })
	out, _ := stderrOf(t, []string{"-h"}, first, second)
	if strings.Contains(out, "count the old way") || !strings.Contains(out, "tally") {
		t.Errorf("help keeps the replaced description:\n%s", out)
	}
}
