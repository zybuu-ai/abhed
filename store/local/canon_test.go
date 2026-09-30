package local

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		`{"b":1, "a":[true,null,"x<y>&"]}`: `{"a":[true,null,"x<y>&"],"b":1}`,
		`{"a":1,"a":2}`:                    `{"a":2}`,
		`  1.50e3 `:                        `1.50e3`,
		``:                                 `null`,
	} {
		got, err := canonical([]byte(in))
		if err != nil || string(got) != want {
			t.Errorf("canonical(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	if got, _ := canonical([]byte(`"\u00e9\u2028"`)); string(got) != "\"\u00e9\\u2028\"" {
		t.Errorf("escapes: %s", got)
	}
	for _, bad := range []string{`{"a":}`, `1 2`, `{"a":1}x`} {
		if _, err := canonical([]byte(bad)); err == nil {
			t.Errorf("canonical(%s) accepted", bad)
		}
	}
}

// FuzzCanonical checks that canonical form is stable: canonical JSON is
// valid, canonicalising it again changes nothing, and a line sealed over it
// checks.
func FuzzCanonical(f *testing.F) {
	for _, s := range []string{`{"b":1,"a":2}`, `[1,"x",{"y":null}]`, `"\ud800"`, `{"k":"<"}`, `-0.0e-10`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := canonical(data)
		if err != nil {
			return
		}
		if !json.Valid(c) {
			t.Fatalf("canonical output is not JSON: %q", c)
		}
		again, err := canonical(c)
		if err != nil || !bytes.Equal(again, c) {
			t.Fatalf("not idempotent: %q -> %q (%v)", c, again, err)
		}
		l := line{Seq: 1, ID: "x", SessionID: "s", Type: "t", Actor: "a", Trust: "trusted", CreatedAt: "2026-09-30T00:00:00Z", Payload: c, Prev: Genesis}
		raw, err := l.seal()
		if err != nil {
			t.Fatal(err)
		}
		if _, why := checkLine(raw); why != "" {
			t.Fatalf("a sealed line does not check: %s: %s", why, raw)
		}
	})
}

// The review's probe: two keys that differ only in invalid bytes would
// collapse into one; such JSON is refused.
func TestCanonicalRefusesInvalidUTF8(t *testing.T) {
	for _, in := range []string{"{\"\xff\":1,\"\xfe\":2}", "{\"t\":\"\xff\xfe\"}"} {
		if _, err := canonical([]byte(in)); err == nil {
			t.Errorf("canonical(%q) accepted", in)
		}
	}
}

// A sealed line whose payload is valid JSON but not canonical fails: keys
// out of order, or a duplicate key. (The mutant without this check survived.)
func TestCheckLineRefusesANonCanonicalPayload(t *testing.T) {
	for _, payload := range []string{`{"b":1,"a":2}`, `{"text":"a","text":"b"}`} {
		l := line{Seq: 1, ID: "a", SessionID: "s-1", Type: "user.message", Actor: "user", Trust: "trusted",
			CreatedAt: "2026-09-30T00:00:00Z", Payload: json.RawMessage(payload), Prev: Genesis}
		raw, err := l.seal()
		if err != nil {
			t.Fatal(err)
		}
		if _, why := checkLine(raw); why == "" {
			t.Errorf("payload %s accepted", payload)
		}
	}
}

// Two lines at one seq, each sealed and chained, fail verify. (The mutant
// without the duplicate check survived.)
func TestVerifyRefusesARepeatedSeq(t *testing.T) {
	prev := Genesis
	var raws [][]byte
	for i, seq := range []int64{1, 2, 2} {
		l := line{Seq: seq, ID: fmt.Sprint("e", i), SessionID: "s-1", Type: "user.message", Actor: "user", Trust: "trusted",
			CreatedAt: "2026-09-30T00:00:00Z", Payload: json.RawMessage(`{"text":"x"}`), Prev: prev}
		raw, _ := l.seal()
		raws = append(raws, raw)
		prev = l.Hash
	}
	rep, _ := verifyLines(raws, "s-1")
	if rep.OK || rep.Line != 3 || rep.FirstBad != 2 {
		t.Fatalf("a repeated seq: %+v", rep)
	}
}
