package index

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// SearchTool exposes the index to the agent as tier-2 retrieval.
//
// The description deliberately tells the model when NOT to use it. Evidence for
// agentic search over embeddings is real but contested, so Abhed routes by
// query shape: identifier-like queries go to grep, natural-language questions
// about behavior come here. Usage is counted so the routing policy can be
// evaluated on real sessions rather than assumed (docs §02 §4).
type SearchTool struct {
	Index *Index
	// Calls counts invocations, reported alongside grep usage to compare tiers.
	Calls atomic.Int64
}

func (*SearchTool) Name() string  { return "search" }
func (*SearchTool) Mutates() bool { return false }

func (*SearchTool) Description() string {
	return "Search the indexed codebase by meaning rather than exact text. " +
		"Use for questions like \"where is authentication handled\" or \"how are retries configured\", " +
		"where you do not know the identifier to search for. " +
		"For a known symbol, error string, or literal, prefer grep — it is exact and needs no index."
}

func (*SearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object",
  "properties":{
    "query":{"type":"string","description":"What you are looking for, in natural language or as identifiers."},
    "limit":{"type":"integer","description":"Maximum results. Default 8."},
    "path_prefix":{"type":"string","description":"Restrict results to paths under this prefix."}
  },
  "required":["query"]
}`)
}

type searchArgs struct {
	Query      string `json:"query"`
	Limit      int    `json:"limit"`
	PathPrefix string `json:"path_prefix"`
}

func (t *SearchTool) Run(ctx context.Context, sess *tools.Session, raw json.RawMessage) tools.Result {
	var a searchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return tools.Result{Content: fmt.Sprintf("Invalid arguments for search: %v", err), IsError: true}
	}
	if strings.TrimSpace(a.Query) == "" {
		return tools.Result{Content: "query is required.", IsError: true}
	}
	if t.Index == nil {
		return tools.Result{
			Content: "No index is available. Use grep and glob instead, or run `abhed index` to build one.",
			IsError: true,
		}
	}

	docs, _, _, _ := t.Index.Stats()
	if docs == 0 {
		return tools.Result{
			Content: "The index is empty. Use grep and glob instead, or run `abhed index` to build one.",
			IsError: true,
		}
	}

	t.Calls.Add(1)

	limit := a.Limit
	if limit <= 0 {
		limit = 8
	}
	hits, err := t.Index.Search(ctx, a.Query, limit*3)
	if err != nil {
		return tools.Result{Content: fmt.Sprintf("Search failed: %v", err), IsError: true}
	}

	// A hit in a file a read rule withholds is left out, its excerpt too:
	// search shows contents, as read would. An index built before the rule
	// may still hold the file.
	withheld := 0
	kept := hits[:0]
	for _, h := range hits {
		if tools.ReadRefused(ctx, h.Doc.Path) {
			withheld++
			continue
		}
		kept = append(kept, h)
	}
	hits = kept

	if a.PathPrefix != "" {
		filtered := hits[:0]
		for _, h := range hits {
			if strings.HasPrefix(h.Doc.Path, a.PathPrefix) || strings.HasPrefix(rel(sess, h.Doc.Path), a.PathPrefix) {
				filtered = append(filtered, h)
			}
		}
		hits = filtered
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}

	note := ""
	if withheld > 0 {
		note = "\n[results in files a read rule withholds are not shown]"
	}
	if len(hits) == 0 {
		return tools.Result{Content: fmt.Sprintf(
			"No results for %q. Try grep with a specific identifier, or broaden the query.%s", a.Query, note)}
	}

	var b strings.Builder
	for i, h := range hits {
		loc := fmt.Sprintf("%s:%d", rel(sess, h.Doc.Path), h.Doc.StartLine)
		if h.Doc.Symbol != "" {
			fmt.Fprintf(&b, "%d. %s  (%s)  [%s]\n", i+1, loc, h.Doc.Symbol, h.Tier)
		} else {
			fmt.Fprintf(&b, "%d. %s  [%s]\n", i+1, loc, h.Tier)
		}
		// A short excerpt lets the model decide whether to read the file, which
		// is cheaper than returning whole chunks it will discard.
		for _, line := range excerpt(h.Doc.Content, 4) {
			fmt.Fprintf(&b, "     %s\n", line)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Use read() on the paths above for full context.")
	return tools.Result{Content: strings.TrimSpace(b.String()) + note}
}

func rel(sess *tools.Session, path string) string {
	if sess == nil {
		return path
	}
	if r, err := filepath.Rel(sess.Root, path); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return path
}

func excerpt(content string, maxLines int) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(line) > 110 {
			line = line[:110] + "…"
		}
		out = append(out, line)
		if len(out) >= maxLines {
			break
		}
	}
	return out
}
