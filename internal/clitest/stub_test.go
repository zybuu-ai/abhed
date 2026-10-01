package clitest

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseScript(t *testing.T) {
	turns, err := ParseScript(`text "Hel"
# a comment
text "lo"
usage in=1200 cached=900 out=7

reasoning "hmm"
tool edit {"path":"a"}
delay 30ms

error 403`)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || len(turns[0]) != 3 || turns[0][2].in != 1200 || turns[0][2].hit != 900 || turns[0][2].out != 7 {
		t.Fatalf("%+v", turns)
	}
	if turns[1][1].name != "edit" || turns[1][1].args != `{"path":"a"}` || turns[1][2].dur != 30*time.Millisecond {
		t.Fatalf("%+v", turns[1])
	}
	if turns[2][0].code != 403 {
		t.Fatalf("%+v", turns[2])
	}
	for _, bad := range []string{`text Hello`, `tool edit {nope`, `usage in=x`, `wobble 1`, `delay soon`} {
		if _, err := ParseScript(Script(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// The stub streams each step as its own event, flushed, and takes one turn
// per request; past the end it says so.
func TestStubStreams(t *testing.T) {
	s := NewStub(t, "text \"a\"\ntool bash {\"command\":\"ls\"}\nusage in=5 out=1\n\nerror 500")
	post := func() (int, string, http.Header) {
		resp, err := http.Post(s.URL()+"/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(bufio.NewReader(resp.Body))
		return resp.StatusCode, string(b), resp.Header
	}
	code, body, _ := post()
	for _, w := range []string{`"content":"a"`, `"name":"bash"`, `"finish_reason":"tool_calls"`, `"prompt_tokens":5`, "data: [DONE]"} {
		if code != 200 || !strings.Contains(body, w) {
			t.Errorf("first turn lacks %s (%d):\n%s", w, code, body)
		}
	}
	code, _, hdr := post()
	if code != 500 || hdr.Get("Retry-After") != "0" {
		t.Errorf("second turn: %d %v", code, hdr)
	}
	if _, body, _ = post(); !strings.Contains(body, "(end of script)") {
		t.Errorf("past the end: %s", body)
	}
	if len(s.Requests()) != 3 || len(s.Deltas()) != 2 || s.Deltas()[0].Text != "a" {
		t.Errorf("requests %d deltas %+v", len(s.Requests()), s.Deltas())
	}
}

func TestNormalizeAndScan(t *testing.T) {
	got := NormalizeText("1 turns · 1200 in / 80 out tokens · took 1.5s ⠙ s-0123456789abcdef01234567 " + strings.Repeat("ab", 32))
	want := "‹n› turns · ‹n› in / ‹n› out tokens · took ‹dur› ‹spin› ‹id› ‹sha›"
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
	if p := ScanGolden("fine text with an openai-compatible endpoint"); len(p) != 0 {
		t.Fatalf("clean text flagged: %v", p)
	}
	if p := ScanGolden("wrote /Users/someone/x and " + CanaryPrefix + "1"); len(p) != 2 {
		t.Fatalf("problems %v", p)
	}
}

func TestP95(t *testing.T) {
	xs := make([]int, 100)
	for i := range xs {
		xs[i] = i + 1
	}
	if P95(xs) != 95 || P95([]int{7}) != 7 {
		t.Fatalf("%d", P95(xs))
	}
}
