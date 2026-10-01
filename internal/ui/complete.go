package ui

import (
	"path"
	"sort"
	"strings"
)

// subsequence scores how well q matches s as an in-order subsequence, the
// way a fuzzy finder does: consecutive runs and matches at the start of a
// word or path segment score higher. ok is false when q is not in s at all.
func subsequence(s, q string) (score int, ok bool) {
	if q == "" {
		return 0, true
	}
	sr, qr := []rune(s), []rune(q)
	j, run, prev := 0, 0, -2
	for i := 0; i < len(sr) && j < len(qr); i++ {
		if sr[i] != qr[j] {
			continue
		}
		score++
		if i == prev+1 {
			run++
			score += 2 * run
		} else {
			run = 0
		}
		if i == 0 || strings.ContainsRune("/._- ", sr[i-1]) {
			score += 3
		}
		prev = i
		j++
	}
	return score, j == len(qr)
}

// fileMatches ranks workspace paths for an "@" mention.
//
// A query matching the file's own name ranks above one that only matches
// its directory, and a shorter path above a longer one, so "@main" offers
// main.go before internal/something/domain_test.go.
func fileMatches(files []string, q string, limit int) []menuItem {
	type cand struct {
		p     string
		score int
	}
	lq := strings.ToLower(q)
	var cs []cand
	for _, f := range files {
		lf := strings.ToLower(f)
		base := strings.ToLower(path.Base(strings.TrimSuffix(f, "/")))
		var sc int
		switch {
		case lq == "":
			sc = 100 - strings.Count(f, "/")*10
		case strings.HasPrefix(base, lq):
			sc = 1000 - len(f)
		case strings.Contains(base, lq):
			sc = 800 - len(f)
		case strings.HasPrefix(lf, lq):
			sc = 700 - len(f)
		case strings.Contains(lf, lq):
			sc = 500 - len(f)
		default:
			s, ok := subsequence(lf, lq)
			if !ok {
				continue
			}
			sc = s*10 - len(f)
		}
		cs = append(cs, cand{f, sc})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].score != cs[j].score {
			return cs[i].score > cs[j].score
		}
		return cs[i].p < cs[j].p
	})
	if len(cs) > limit {
		cs = cs[:limit]
	}
	out := make([]menuItem, 0, len(cs))
	for _, c := range cs {
		dir := strings.HasSuffix(c.p, "/")
		detail := ""
		if dir {
			detail = "directory"
		}
		out = append(out, menuItem{label: "@" + c.p, detail: detail, insert: c.p, dir: dir})
	}
	return out
}
