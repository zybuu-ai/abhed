package ui

import (
	"os"
	"strings"
	"testing"
)

// TestEveryCommandIsReachableFromTheMenu pins the thing that was broken: the
// help text was a hand-maintained string, so a command could exist in help and
// be uncompletable, or completable and undocumented.
func TestEveryCommandIsReachableFromTheMenu(t *testing.T) {
	help := HelpText(NewStyle(os.Stdout))
	for _, c := range Commands {
		if !strings.Contains(help, c.Name) {
			t.Errorf("%s is missing from /help", c.Name)
		}
		if got := MatchCommands(c.Name); len(got) == 0 {
			t.Errorf("%s does not complete from its own name", c.Name)
		}
	}
	// A bare slash offers everything: that is what makes the menu discoverable.
	if n := len(MatchCommands("/")); n != len(Commands) {
		t.Errorf("a bare / offered %d of %d commands", n, len(Commands))
	}
	// Not a command, so no menu.
	if got := MatchCommands("ls /tmp"); got != nil {
		t.Errorf("a path should not open the command menu, got %d", len(got))
	}
}

func TestCommonPrefixCompletesAsFarAsItCan(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/q", "/quit"},       // unique
		{"/co", "/co"},        // /compact and /cost share only the stem
		{"/comp", "/compact"}, // now unambiguous
		{"/zzz", ""},          // nothing
	}
	for _, c := range cases {
		got := CommonPrefix(MatchCommands(c.in))
		if got != c.want {
			t.Errorf("CommonPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestThinkingVerbsDescribeTheHarness keeps the vocabulary on-brand. These are
// shown to an engineer on every turn; a verb that reads as a chatbot's
// personality ages badly on the hundredth run.
func TestThinkingVerbsDescribeTheHarness(t *testing.T) {
	banned := []string{"pondering", "musing", "vibing", "cooking", "brewing", "noodling"}
	for _, v := range Verbs() {
		for _, b := range banned {
			if strings.EqualFold(v, b) {
				t.Errorf("%q reads as personality, not infrastructure", v)
			}
		}
		if v == "" || strings.ToUpper(v[:1]) != v[:1] {
			t.Errorf("verb %q should be capitalised for display", v)
		}
	}
	if len(Verbs()) < 8 {
		t.Errorf("too few verbs (%d): a long wait repeats visibly", len(Verbs()))
	}
}
