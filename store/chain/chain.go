// Package chain is the storage-neutral view of Abhed's hash-chained records:
// where a chain stands (its head), and a check that a range of it still
// holds. An edition can sign and anchor heads through it without knowing
// where the record is kept, and check them again later.
//
// A head says nothing on its own: the store that gives it is the one being
// checked. Its value comes from being kept somewhere the store's owner cannot
// rewrite, and from VerifyRange later finding the same hash at the same place.
package chain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Genesis is what a chain's first link names as the one before it.
var Genesis = strings.Repeat("0", 64)

// Head is where one chain stands: how many links it holds, and the hash of
// the last. An empty chain has Pos 0 and no Hash.
type Head struct {
	// Chain names the kind of record, such as "postgres/events" (one chain
	// per session) or "postgres/sandbox_audit" (one per tenant).
	Chain  string `json:"chain"`
	Tenant string `json:"tenant"`
	// Scope is the key the chain is kept per: a session id, or a tenant.
	Scope string `json:"scope"`
	Pos   int64  `json:"pos"`
	Hash  string `json:"hash"`
}

// Range is what a check of positions From to To found.
type Range struct {
	From, To int64
	// Before is the hash the range links to: the one at From-1, or Genesis.
	// A check that goes on from an anchored head compares it to that head.
	Before string
	// Hash is the hash at To as the check recomputed it, to compare with a
	// head kept elsewhere. It is the last good link's when the range fails.
	Hash string
	// FirstBad is the first position that does not hold, and Reason why;
	// zero and empty when every one does.
	FirstBad int64
	Reason   string
}

// OK reports whether every position in the range holds.
func (r Range) OK() bool { return r.FirstBad == 0 && r.Reason == "" }

// Source is a store whose records are chained. Head reads where a chain
// stands without checking it; VerifyRange recomputes positions from to to
// (to 0 is the head) and reports the first that does not hold. Both are
// stable for an edition to build on.
type Source interface {
	Head(ctx context.Context, scope string) (Head, error)
	VerifyRange(ctx context.Context, scope string, from, to int64) (Range, error)
}

// Link is one position of a chain as a store holds it. Row is exactly the
// text the hash covers, after Prev and a newline.
type Link struct {
	Pos  int64  `json:"pos"`
	Prev string `json:"prev"`
	Hash string `json:"hash"`
	Row  string `json:"row"`
}

// Linker is a Source that hands out its links as stored, from to to (to 0
// is the head), unchecked and in order, for an export of a range; fn
// returning false stops it. A store whose positions are not Links (a line
// file hashed whole) is not one.
type Linker interface {
	Links(ctx context.Context, scope string, from, to int64, fn func(Link) bool) error
}

// LinkHash is the hash of a link whose predecessor's hash is prev: SHA-256,
// in hex, of prev, a newline and row.
func LinkHash(prev, row string) string {
	sum := sha256.Sum256([]byte(prev + "\n" + row))
	return hex.EncodeToString(sum[:])
}

// Walker checks links in the order they are read, so a long chain is never
// held in memory. Give it every link from its first position on.
type Walker struct {
	r    Range
	want int64
	prev string
	seen func(pos int64, hash string)
}

// NewWalker starts a check at position from, whose link must name before
// (Genesis when from is 1). seen, when set, is given each position's hash
// once it is known to hold.
func NewWalker(from int64, before string, seen func(pos int64, hash string)) *Walker {
	if from < 1 {
		from = 1
	}
	return &Walker{r: Range{From: from, Before: before, Hash: before}, want: from, prev: before, seen: seen}
}

// Next checks one link and reports whether the chain still holds.
func (w *Walker) Next(l Link) bool {
	if !w.r.OK() {
		return false
	}
	switch {
	case l.Pos != w.want:
		w.fail(w.want, fmt.Sprintf("position %d is missing or out of order (found %d)", w.want, l.Pos))
	case l.Prev != w.prev:
		w.fail(l.Pos, fmt.Sprintf("position %d does not link to the one before it: a link was removed, added or reordered", l.Pos))
	case LinkHash(l.Prev, l.Row) != l.Hash:
		w.fail(l.Pos, fmt.Sprintf("position %d does not match its hash: it was changed", l.Pos))
	default:
		w.prev, w.want = l.Hash, w.want+1
		w.r.To, w.r.Hash = l.Pos, l.Hash
		if w.seen != nil {
			w.seen(l.Pos, l.Hash)
		}
		return true
	}
	return false
}

func (w *Walker) fail(pos int64, why string) {
	w.r.FirstBad, w.r.Reason = pos, why
}

// Done ends the check. With to above zero the range must reach it; a chain
// that stops short had links cut from its end.
func (w *Walker) Done(to int64) Range {
	r := w.r
	if r.OK() && to > 0 && w.want <= to {
		r.FirstBad = w.want
		r.Reason = fmt.Sprintf("the chain ends at position %d, before %d: links were cut from its end", w.want-1, to)
	}
	if r.To == 0 {
		r.To = w.want - 1
	}
	if to > 0 {
		r.To = to
	}
	return r
}
