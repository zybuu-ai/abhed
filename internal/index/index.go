// Package index provides on-prem retrieval over a codebase.
//
// Position (docs/architecture/02-system-architecture.md §4): the evidence that
// agentic search beats embedding retrieval is a strong vendor preference, not a
// settled finding — the strongest supporting statistic was refuted, and a
// counter-result puts keyword search via tool use at >90% of RAG performance.
//
// So Abhed is agentic-first with retrieval as an accelerator, and it MEASURES
// which tier resolved each query rather than assuming. The tiers:
//
//	Tier 0  glob/grep              always available, no index (internal/tools)
//	Tier 1  symbol index           cheap structural prior, built here
//	Tier 2  hybrid BM25 + vector   large corpora, built here
//
// Everything runs on-prem with no external services: BM25 is local, and the
// embedding backend is whatever OpenAI-compatible endpoint you already run.
package index

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zybuu-ai/abhed/internal/tools"
)

// Doc is one indexed chunk.
type Doc struct {
	ID       string
	Path     string
	Language string
	// Symbol is the enclosing function/type when known, which makes a hit
	// actionable rather than just a line number.
	Symbol    string
	StartLine int
	EndLine   int
	Content   string
	// Vector is optional; nil when no embedder is configured.
	Vector []float32
}

// Hit is a scored result with the tier that produced it, so routing can be
// evaluated empirically rather than assumed.
type Hit struct {
	Doc   Doc
	Score float64
	Tier  string // "symbol" | "bm25" | "vector" | "hybrid"
}

// Embedder produces vectors. Implemented over any OpenAI-compatible
// /embeddings endpoint, so the same on-prem stack serving the model can serve
// retrieval.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
}

// Index is an on-prem hybrid index.
type Index struct {
	mu sync.RWMutex

	docs   []Doc
	byPath map[string][]int

	// BM25 state.
	df       map[string]int
	docTerms []map[string]int
	docLen   []int
	avgLen   float64

	// Symbol lookup: identifier -> doc indices.
	symbols map[string][]int

	embedder Embedder
	root     string
	built    time.Time
}

func New(root string) *Index {
	return &Index{
		byPath:  make(map[string][]int),
		df:      make(map[string]int),
		symbols: make(map[string][]int),
		root:    root,
	}
}

func (ix *Index) WithEmbedder(e Embedder) *Index {
	ix.embedder = e
	return ix
}

// Stats reports index size, for `abhed doctor` and capacity planning.
func (ix *Index) Stats() (docs int, terms int, vectors int, built time.Time) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	for _, d := range ix.docs {
		if len(d.Vector) > 0 {
			vectors++
		}
	}
	return len(ix.docs), len(ix.df), vectors, ix.built
}

// BuildOptions controls indexing.
type BuildOptions struct {
	Extensions  []string
	MaxFileSize int64
	// Embed populates vectors. Off by default: for most repos the symbol and
	// BM25 tiers answer the question, and embedding a monorepo is expensive.
	Embed bool
}

func DefaultBuildOptions() BuildOptions {
	return BuildOptions{
		Extensions: []string{
			".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".java", ".rs", ".rb",
			".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".php", ".swift", ".kt",
			".scala", ".sh", ".sql", ".md", ".rst", ".txt", ".yaml", ".yml",
		},
		MaxFileSize: 2 << 20,
	}
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "__pycache__": true, ".venv": true,
	"venv": true, ".next": true, ".cache": true, ".abhed": true,
}

// Build walks the workspace and indexes it. Incremental updates go through
// Update; a full rebuild is only needed on first run.
func (ix *Index) Build(ctx context.Context, opts BuildOptions) error {
	exts := make(map[string]bool, len(opts.Extensions))
	for _, e := range opts.Extensions {
		exts[e] = true
	}

	var docs []Doc
	state := tools.NewStateSet(ix.root)
	// Files are read under the root held open, so a folder swapped for a link
	// during the walk cannot lead a read out of it.
	within, err := state.Confine(ix.root)
	if err != nil {
		return err
	}
	defer within.Close()
	err = filepath.WalkDir(ix.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the index
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != ix.root && (skipDirs[d.Name()] || state.HasEntry(d)) {
				return filepath.SkipDir
			}
			return nil
		}
		// Links are not followed, since one can lead out of the workspace or
		// into Abhed's state, and nor is a state file under another name.
		if d.Type()&os.ModeSymlink != 0 || state.HasEntry(d) {
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > opts.MaxFileSize {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the index
		}
		rel, err := filepath.Rel(ix.root, path)
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the index
		}
		content, err := within.ReadEntry(rel, info)
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal to the index
		}
		docs = append(docs, chunkFile(path, string(content))...)
		return nil
	})
	if err != nil {
		return err
	}

	ix.mu.Lock()
	ix.docs = docs
	ix.rebuildLocked()
	ix.built = time.Now()
	ix.mu.Unlock()

	if opts.Embed && ix.embedder != nil {
		return ix.embedAll(ctx)
	}
	return nil
}

// Update re-indexes a single file, so editing during a session does not require
// a full rebuild — the property that makes indexing viable at monorepo scale.
func (ix *Index) Update(ctx context.Context, path string) error {
	// Callers resolve paths through the session boundary before they get
	// here, but an exported method should not rely on every future caller
	// remembering to. The index is scoped to its root; a path outside it is
	// refused rather than read.
	if ix.root != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(ix.root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("index: %s is outside the indexed root", path)
		}
		if real := tools.RealPath(abs); !strings.HasPrefix(real+string(filepath.Separator), tools.RealPath(ix.root)+string(filepath.Separator)) {
			return fmt.Errorf("index: %s leads outside the indexed root", path)
		}
		if tools.IsState(abs, ix.root) {
			return fmt.Errorf("index: %s is Abhed's own state", path)
		}
	}
	content, err := ix.readConfined(path)
	if err != nil {
		return err
	}
	fresh := chunkFile(path, string(content))

	ix.mu.Lock()
	defer ix.mu.Unlock()

	kept := make([]Doc, 0, len(ix.docs))
	for _, d := range ix.docs {
		if d.Path != path {
			kept = append(kept, d)
		}
	}
	kept = append(kept, fresh...)
	ix.docs = kept
	ix.rebuildLocked()
	return nil
}

// readConfined reads a file under the root, with links followed only inside
// it and Abhed's state refused; without a root it reads as asked.
func (ix *Index) readConfined(path string) ([]byte, error) {
	if ix.root == "" {
		return os.ReadFile(path)
	}
	c, err := tools.NewStateSet(ix.root).Confine(ix.root, tools.RealPath(ix.root))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return c.ReadFile(abs)
}

// rebuildLocked recomputes the derived structures. Caller holds the write lock.
func (ix *Index) rebuildLocked() {
	ix.byPath = make(map[string][]int, len(ix.docs))
	ix.df = make(map[string]int)
	ix.symbols = make(map[string][]int)
	ix.docTerms = make([]map[string]int, len(ix.docs))
	ix.docLen = make([]int, len(ix.docs))

	total := 0
	for i, d := range ix.docs {
		ix.byPath[d.Path] = append(ix.byPath[d.Path], i)

		terms := tokenize(d.Content)
		counts := make(map[string]int, len(terms))
		for _, t := range terms {
			counts[t]++
		}
		ix.docTerms[i] = counts
		ix.docLen[i] = len(terms)
		total += len(terms)
		for t := range counts {
			ix.df[t]++
		}

		if d.Symbol != "" {
			key := strings.ToLower(d.Symbol)
			ix.symbols[key] = append(ix.symbols[key], i)
		}
	}
	if len(ix.docs) > 0 {
		ix.avgLen = float64(total) / float64(len(ix.docs))
	}
}

func (ix *Index) embedAll(ctx context.Context) error {
	ix.mu.RLock()
	texts := make([]string, len(ix.docs))
	for i, d := range ix.docs {
		texts[i] = d.Content
	}
	ix.mu.RUnlock()

	const batch = 64
	for start := 0; start < len(texts); start += batch {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := min(start+batch, len(texts))
		vecs, err := ix.embedder.Embed(ctx, texts[start:end])
		if err != nil {
			return fmt.Errorf("embedding batch %d-%d: %w", start, end, err)
		}
		ix.mu.Lock()
		for i, v := range vecs {
			if start+i < len(ix.docs) {
				ix.docs[start+i].Vector = v
			}
		}
		ix.mu.Unlock()
	}
	return nil
}

// Search runs the tiers and fuses their results.
//
// Reciprocal rank fusion is used rather than score normalization: BM25 and
// cosine scores are not on comparable scales, and RRF only needs rank order,
// which makes it robust when one tier is missing entirely.
func (ix *Index) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	ix.mu.RLock()
	haveVectors := false
	for _, d := range ix.docs {
		if len(d.Vector) > 0 {
			haveVectors = true
			break
		}
	}
	ix.mu.RUnlock()

	rankings := [][]Hit{
		ix.searchSymbols(query, limit*2),
		ix.searchBM25(query, limit*2),
	}
	if haveVectors && ix.embedder != nil {
		vec, err := ix.searchVector(ctx, query, limit*2)
		if err == nil {
			rankings = append(rankings, vec)
		}
	}
	return fuse(rankings, limit), nil
}

// searchSymbols matches identifiers exactly. An agent looking for "handleAuth"
// wants the definition, not documents that merely mention it.
func (ix *Index) searchSymbols(query string, limit int) []Hit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	var hits []Hit
	seen := map[int]bool{}
	for _, term := range tokenize(query) {
		for _, idx := range ix.symbols[term] {
			if seen[idx] {
				continue
			}
			seen[idx] = true
			score := 1.0
			if strings.EqualFold(ix.docs[idx].Symbol, query) {
				score = 2.0 // exact whole-query match ranks above partial
			}
			hits = append(hits, Hit{Doc: ix.docs[idx], Score: score, Tier: "symbol"})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// searchBM25 is the lexical tier: strong on identifiers and error strings,
// which is most of what a coding agent actually searches for.
func (ix *Index) searchBM25(query string, limit int) []Hit {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	const k1, b = 1.2, 0.75
	n := float64(len(ix.docs))
	if n == 0 {
		return nil
	}

	scores := make(map[int]float64)
	for _, term := range tokenize(query) {
		df := float64(ix.df[term])
		if df == 0 {
			continue
		}
		idf := math.Log(1 + (n-df+0.5)/(df+0.5))
		for i, counts := range ix.docTerms {
			tf := float64(counts[term])
			if tf == 0 {
				continue
			}
			norm := tf * (k1 + 1) / (tf + k1*(1-b+b*float64(ix.docLen[i])/ix.avgLen))
			scores[i] += idf * norm
		}
	}

	hits := make([]Hit, 0, len(scores))
	for i, s := range scores {
		hits = append(hits, Hit{Doc: ix.docs[i], Score: s, Tier: "bm25"})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// searchVector is the semantic tier: earns its place on natural-language
// queries over docs and runbooks, where the wording will not match.
func (ix *Index) searchVector(ctx context.Context, query string, limit int) ([]Hit, error) {
	vecs, err := ix.embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	q := vecs[0]

	ix.mu.RLock()
	defer ix.mu.RUnlock()

	hits := make([]Hit, 0, len(ix.docs))
	for _, d := range ix.docs {
		if len(d.Vector) == 0 {
			continue
		}
		hits = append(hits, Hit{Doc: d, Score: cosine(q, d.Vector), Tier: "vector"})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// fuse combines rankings with reciprocal rank fusion.
func fuse(rankings [][]Hit, limit int) []Hit {
	const k = 60.0
	type agg struct {
		hit   Hit
		score float64
		tiers []string
	}
	combined := map[string]*agg{}

	for _, ranking := range rankings {
		for rank, h := range ranking {
			a, found := combined[h.Doc.ID]
			if !found {
				a = &agg{hit: h}
				combined[h.Doc.ID] = a
			}
			a.score += 1.0 / (k + float64(rank+1))
			a.tiers = append(a.tiers, h.Tier)
		}
	}

	out := make([]Hit, 0, len(combined))
	for _, a := range combined {
		h := a.hit
		h.Score = a.score
		h.Tier = strings.Join(dedupe(a.tiers), "+")
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

var tokenPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*|\d+`)

// tokenize splits text into search terms, additionally splitting camelCase and
// snake_case so a query for "handle auth" finds handleAuth.
func tokenize(s string) []string {
	raw := tokenPattern.FindAllString(s, -1)
	out := make([]string, 0, len(raw)*2)
	for _, tok := range raw {
		lower := strings.ToLower(tok)
		if len(lower) > 1 {
			out = append(out, lower)
		}
		for _, part := range splitIdentifier(tok) {
			if p := strings.ToLower(part); len(p) > 1 && p != lower {
				out = append(out, p)
			}
		}
	}
	return out
}

func splitIdentifier(s string) []string {
	var parts []string
	var cur strings.Builder
	for i, r := range s {
		if r == '_' || r == '-' {
			if cur.Len() > 0 {
				parts = append(parts, cur.String())
				cur.Reset()
			}
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' && cur.Len() > 0 {
			parts = append(parts, cur.String())
			cur.Reset()
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}
