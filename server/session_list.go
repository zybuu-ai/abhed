package server

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxListLimit is the most sessions one page of GET /v1/sessions holds.
const maxListLimit = 200

// NextCursorHeader carries the cursor of the next page of GET /v1/sessions,
// absent on the last. The body stays a plain array, as it always was.
const NextCursorHeader = "X-Next-Cursor"

// listPage is what GET /v1/sessions was asked for: a search, a page size
// and where the page starts. A zero listPage is the whole list.
type listPage struct {
	q      string
	limit  int
	cursor *listCursor
}

// listCursor is the last row of the page before: the list is ordered by
// last activity, then id, both descending.
type listCursor struct {
	at time.Time
	id string
}

func parseListPage(v url.Values) (listPage, error) {
	p := listPage{q: strings.ToLower(strings.TrimSpace(v.Get("q")))}
	if len(p.q) > 200 {
		return p, errors.New("q is longer than 200 characters")
	}
	if raw := v.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListLimit {
			return p, errors.New("limit must be a number from 1 to 200")
		}
		p.limit = n
	}
	if raw := v.Get("cursor"); raw != "" {
		c, err := decodeCursor(raw)
		if err != nil {
			return p, errors.New("cursor is not one this server gave")
		}
		p.cursor = &c
	}
	return p, nil
}

func encodeCursor(c listCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.at.UnixNano(), 10) + ":" + c.id))
}

func decodeCursor(raw string) (listCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return listCursor{}, err
	}
	at, id, ok := strings.Cut(string(b), ":")
	if !ok || id == "" {
		return listCursor{}, errors.New("malformed cursor")
	}
	ns, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return listCursor{}, err
	}
	return listCursor{at: time.Unix(0, ns), id: id}, nil
}

// before reports whether s comes after the cursor in the list's order.
func (c listCursor) before(s sessionSummary) bool {
	if !s.Updated.Equal(c.at) {
		return s.Updated.Before(c.at)
	}
	return s.ID < c.id
}

// matches reports whether a session's title or opening request holds q,
// ignoring case. An id is matched too, so a pasted id finds its session.
func (p listPage) matches(s sessionSummary) bool {
	if p.q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s.Title), p.q) ||
		strings.Contains(strings.ToLower(s.Prompt), p.q) ||
		strings.HasPrefix(strings.ToLower(s.ID), p.q)
}

// writeSessionPage sorts the sessions by last activity, newest first, keeps
// those the page asks for, and names the next page's cursor in a header.
func writeSessionPage(w http.ResponseWriter, all []sessionSummary, p listPage) {
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Updated.Equal(all[j].Updated) {
			return all[i].Updated.After(all[j].Updated)
		}
		return all[i].ID > all[j].ID
	})
	out := all[:0]
	for _, s := range all {
		if p.matches(s) && (p.cursor == nil || p.cursor.before(s)) {
			out = append(out, s)
		}
	}
	if p.limit > 0 && len(out) > p.limit {
		out = out[:p.limit]
		last := out[len(out)-1]
		w.Header().Set(NextCursorHeader, encodeCursor(listCursor{at: last.Updated, id: last.ID}))
	}
	WriteJSON(w, http.StatusOK, out)
}

// lastAtter is a store that says when a session's conversation last went on.
type lastAtter interface {
	LastAt(sessionID string) (time.Time, bool)
}

// lastActivity is when session id's conversation last went on, where the
// store under any tap can say.
func (s *Server) lastActivity(id string) (time.Time, bool) {
	if la, ok := s.under().(lastAtter); ok {
		return la.LastAt(id)
	}
	return time.Time{}, false
}
