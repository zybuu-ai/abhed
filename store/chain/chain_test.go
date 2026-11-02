package chain

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// links builds a chain of n links whose rows are small JSON objects.
func links(n int) []Link {
	out := make([]Link, 0, n)
	prev := Genesis
	for i := 1; i <= n; i++ {
		row := fmt.Sprintf(`{"seq": %d, "type": "agent.message"}`, i)
		h := LinkHash(prev, row)
		out = append(out, Link{Pos: int64(i), Prev: prev, Hash: h, Row: row})
		prev = h
	}
	return out
}

func export(t *testing.T, ls []Link) []string {
	t.Helper()
	var b bytes.Buffer
	w := NewWriter(&b, Header{Chain: "postgres/events", Tenant: "t", Scope: "s", From: 1, ExportedAt: time.Unix(0, 0).UTC()})
	for _, l := range ls {
		if err := w.Link(l); err != nil {
			t.Fatal(err)
		}
	}
	last := ls[len(ls)-1]
	if err := w.Close(Head{Chain: "postgres/events", Tenant: "t", Scope: "s", Pos: last.Pos, Hash: last.Hash}); err != nil {
		t.Fatal(err)
	}
	return strings.SplitAfter(strings.TrimSuffix(b.String(), "\n"), "\n")
}

func verify(t *testing.T, lines []string, heads map[int64]string) ExportReport {
	t.Helper()
	rep, _, err := VerifyExport(strings.NewReader(strings.Join(lines, "")), heads)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestWalkerHoldsAWholeChain(t *testing.T) {
	ls := links(5)
	w := NewWalker(1, Genesis, nil)
	for _, l := range ls {
		if !w.Next(l) {
			t.Fatalf("link %d refused", l.Pos)
		}
	}
	r := w.Done(5)
	if !r.OK() || r.To != 5 || r.Hash != ls[4].Hash {
		t.Fatalf("range = %+v", r)
	}
	// A range from the middle links to the hash before it.
	w = NewWalker(3, ls[1].Hash, nil)
	for _, l := range ls[2:] {
		w.Next(l)
	}
	if r := w.Done(0); !r.OK() || r.From != 3 || r.To != 5 || r.Before != ls[1].Hash {
		t.Fatalf("middle range = %+v", r)
	}
}

// B4: every kind of tamper with an export is found with nothing but the file.
func TestExportTamperIsDetectedOffline(t *testing.T) {
	ls := links(6)
	whole := export(t, ls)
	if r := verify(t, whole, nil); !r.OK() || r.Links != 6 || r.Head == nil || r.Hash != ls[5].Hash {
		t.Fatalf("an untouched export fails: %+v", r)
	}
	edited := append([]string{}, whole...)
	edited[3] = strings.Replace(edited[3], "agent.message", "agent.messagf", 1)
	// The edited row with its hash recomputed: the next link still names the old one.
	rechained := append([]string{}, whole...)
	l3 := ls[2]
	l3.Row = strings.Replace(l3.Row, "agent.message", "agent.messagf", 1)
	l3.Hash = LinkHash(l3.Prev, l3.Row)
	rechained[3] = fmt.Sprintf(`{"pos":3,"prev":%q,"hash":%q,"row":%q}`+"\n", l3.Prev, l3.Hash, l3.Row)
	cases := map[string]struct {
		lines []string
		heads map[int64]string
		at    int64
	}{
		"edit a row":                   {edited, nil, 3},
		"edit a row and its hash":      {rechained, nil, 4},
		"delete a row":                 {append(append([]string{}, whole[:3]...), whole[4:]...), nil, 3},
		"reorder two rows":             {append(append(append([]string{}, whole[:2]...), whole[3], whole[2]), whole[4:]...), nil, 2},
		"truncate, trailer kept":       {append(append([]string{}, whole[:5]...), whole[7]), nil, 5},
		"truncate, trailer gone":       {whole[:5], nil, 5},
		"truncate to the header":       {whole[:1], nil, 1},
		"duplicate a row":              {append(append([]string{}, whole[:3]...), whole[2:]...), nil, 3},
		"a line after the trailer":     {append(append([]string{}, whole...), whole[1]), nil, 7},
		"truncate past a witness head": {whole[:4], map[int64]string{5: ls[4].Hash}, 4},
	}
	for name, c := range cases {
		r := verify(t, c.lines, c.heads)
		if r.OK() {
			t.Errorf("%s: verified OK", name)
			continue
		}
		if r.FirstBad != c.at {
			t.Errorf("%s: first bad %d (%s), want %d", name, r.FirstBad, r.Reason, c.at)
		}
	}
}

// A whole chain recomputed after an edit, trailer and all, passes on its own
// and fails against a head kept elsewhere.
func TestARecomputedExportFailsOnlyAgainstAKeptHead(t *testing.T) {
	ls := links(4)
	kept := ls[3].Hash
	forged := make([]Link, len(ls))
	prev := Genesis
	for i, l := range ls {
		l.Row = strings.Replace(l.Row, "agent.message", "agent.forged", 1)
		l.Prev, l.Hash = prev, LinkHash(prev, l.Row)
		prev = l.Hash
		forged[i] = l
	}
	lines := export(t, forged)
	if r := verify(t, lines, nil); !r.OK() {
		t.Fatalf("a consistent forgery should verify on its own: %+v", r)
	}
	if r := verify(t, lines, map[int64]string{4: kept}); r.OK() || r.FirstBad != 4 {
		t.Fatalf("against the kept head: %+v", r)
	}
}

func TestIsExport(t *testing.T) {
	lines := export(t, links(1))
	if !IsExport([]byte(lines[0])) {
		t.Fatal("an export's header is not recognised")
	}
	if IsExport([]byte(`{"seq":1,"id":"x"}`)) || IsExport([]byte("not json")) {
		t.Fatal("a record line taken for an export")
	}
}
