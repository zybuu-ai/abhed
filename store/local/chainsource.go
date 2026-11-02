package local

import (
	"context"
	"fmt"

	"github.com/zybuu-ai/abhed/store/chain"
)

// Chain is the local record as a chain.Source whose scope is a session id,
// and whose positions are a session's lines.
func (s *Store) Chain() chain.Source { return localChain{s} }

type localChain struct{ s *Store }

// Head is the session's head as last synced, which is what a witness is sent.
func (c localChain) Head(_ context.Context, scope string) (chain.Head, error) {
	if err := checkID("session", scope); err != nil {
		return chain.Head{}, err
	}
	h, _ := c.s.readHead(scope)
	return chain.Head{Chain: "local/session", Tenant: c.s.tenant, Scope: scope, Pos: h.Lines, Hash: h.Hash}, nil
}

// VerifyRange checks the whole session, as Verify does, and reports whether
// lines from to to (0 is the last) are among those that hold.
func (c localChain) VerifyRange(_ context.Context, scope string, from, to int64) (chain.Range, error) {
	if from < 1 {
		from = 1
	}
	rep, err := c.s.Verify(scope)
	if err != nil {
		return chain.Range{}, err
	}
	if to <= 0 {
		to = rep.Head.Lines
	}
	r := chain.Range{From: from, To: to, Before: Genesis}
	if !rep.OK && (rep.Line == 0 || int64(rep.Line) <= to) {
		r.FirstBad, r.Reason = max(int64(rep.Line), from), rep.Reason
		return r, nil
	}
	hs, err := c.s.hashesAt(scope, []int64{from - 1, to})
	if err != nil {
		return chain.Range{}, err
	}
	if from > 1 {
		r.Before = hs[from-1]
	}
	h, ok := hs[to]
	if !ok {
		r.FirstBad = to
		r.Reason = fmt.Sprintf("the record holds no line %d: lines were cut from its end", to)
		return r, nil
	}
	r.Hash = h
	return r, nil
}
