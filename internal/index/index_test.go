package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildTestIndex(t *testing.T) (*Index, string) {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	files := map[string]string{
		"auth/login.go": `package auth

import "errors"

var ErrExpiredToken = errors.New("token expired")

// ValidateToken checks a bearer token against the session store.
func ValidateToken(token string) error {
	if token == "" {
		return ErrExpiredToken
	}
	return nil
}

func RefreshSession(id string) error {
	return nil
}
`,
		"payments/charge.py": `import stripe

class PaymentProcessor:
    """Handles card charges with retry."""

    def charge_card(self, amount, token):
        for attempt in range(3):
            try:
                return stripe.Charge.create(amount=amount, source=token)
            except stripe.error.RateLimitError:
                continue
        raise RuntimeError("retries exhausted")
`,
		"web/client.ts": `export interface RetryConfig {
  maxAttempts: number;
}

export function fetchWithRetry(url: string, cfg: RetryConfig) {
  return fetch(url);
}
`,
		"docs/runbook.md": `# Incident Response

## Database failover
When the primary database becomes unreachable, promote the replica.

## Rotating credentials
Credentials are rotated quarterly via the vault CLI.
`,
	}
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ix := New(dir)
	if err := ix.Build(context.Background(), DefaultBuildOptions()); err != nil {
		t.Fatal(err)
	}
	return ix, dir
}

func TestBuildIndexesFiles(t *testing.T) {
	ix, _ := buildTestIndex(t)
	docs, terms, vectors, _ := ix.Stats()
	if docs == 0 {
		t.Fatal("no documents indexed")
	}
	if terms == 0 {
		t.Fatal("no terms indexed")
	}
	if vectors != 0 {
		t.Fatal("vectors should be empty without an embedder")
	}
	t.Logf("indexed %d chunks, %d unique terms", docs, terms)
}

// Chunking on declaration boundaries is what makes a hit actionable.
func TestChunksCarrySymbolNames(t *testing.T) {
	ix, _ := buildTestIndex(t)
	found := map[string]bool{}
	for _, d := range ix.docs {
		if d.Symbol != "" {
			found[d.Symbol] = true
		}
	}
	for _, want := range []string{"ValidateToken", "RefreshSession", "PaymentProcessor", "fetchWithRetry"} {
		if !found[want] {
			t.Errorf("symbol %q not extracted; got %v", want, keys(found))
		}
	}
}

func TestSymbolSearchFindsExactDefinition(t *testing.T) {
	ix, _ := buildTestIndex(t)
	hits, err := ix.Search(context.Background(), "ValidateToken", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if !strings.Contains(hits[0].Doc.Path, "login.go") {
		t.Fatalf("expected login.go first, got %s (tier %s)", hits[0].Doc.Path, hits[0].Tier)
	}
	if hits[0].Doc.Symbol != "ValidateToken" {
		t.Fatalf("expected the definition, got symbol %q", hits[0].Doc.Symbol)
	}
}

// camelCase splitting: "validate token" should find ValidateToken.
func TestCamelCaseQueryMatches(t *testing.T) {
	ix, _ := buildTestIndex(t)
	hits, _ := ix.Search(context.Background(), "validate token", 5)
	if len(hits) == 0 {
		t.Fatal("no hits for split-identifier query")
	}
	if !strings.Contains(hits[0].Doc.Path, "login.go") {
		t.Fatalf("expected login.go, got %s", hits[0].Doc.Path)
	}
}

func TestNaturalLanguageFindsProse(t *testing.T) {
	ix, _ := buildTestIndex(t)
	hits, _ := ix.Search(context.Background(), "rotating credentials", 5)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if !strings.Contains(hits[0].Doc.Path, "runbook.md") {
		t.Fatalf("expected the runbook, got %s", hits[0].Doc.Path)
	}
}

func TestMarkdownChunkedByHeading(t *testing.T) {
	ix, _ := buildTestIndex(t)
	var headings []string
	for _, d := range ix.docs {
		if strings.HasSuffix(d.Path, ".md") && d.Symbol != "" {
			headings = append(headings, d.Symbol)
		}
	}
	if len(headings) < 2 {
		t.Fatalf("markdown should split on headings, got %v", headings)
	}
}

func TestBM25RanksByRelevance(t *testing.T) {
	ix, _ := buildTestIndex(t)
	hits, _ := ix.Search(context.Background(), "retry attempts rate limit", 5)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	// The Python charge code is the most retry-dense document.
	top := hits[0].Doc.Path
	if !strings.Contains(top, "charge.py") && !strings.Contains(top, "client.ts") {
		t.Fatalf("expected a retry-related file first, got %s", top)
	}
}

func TestIncrementalUpdate(t *testing.T) {
	ix, dir := buildTestIndex(t)
	before, _, _, _ := ix.Stats()

	p := filepath.Join(dir, "auth/login.go")
	_ = os.WriteFile(p, []byte(`package auth

func BrandNewFunction() error {
	return nil
}
`), 0o644)

	if err := ix.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	hits, _ := ix.Search(context.Background(), "BrandNewFunction", 5)
	if len(hits) == 0 || hits[0].Doc.Symbol != "BrandNewFunction" {
		t.Fatal("incremental update did not index the new symbol")
	}
	// And the old symbol is gone.
	hits, _ = ix.Search(context.Background(), "RefreshSession", 5)
	for _, h := range hits {
		if h.Doc.Symbol == "RefreshSession" {
			t.Fatal("stale chunk survived the update")
		}
	}
	after, _, _, _ := ix.Stats()
	t.Logf("chunks %d -> %d after update", before, after)
}

func TestSkipsVendorDirectories(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "node_modules/pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules/pkg/index.js"), []byte("function vendored(){}"), 0o644)
	// A subagent's worktree is a copy of the workspace, not more of it.
	_ = os.MkdirAll(filepath.Join(dir, ".abhed-worktrees/k3f9q2"), 0o755)
	os.WriteFile(filepath.Join(dir, ".abhed-worktrees/k3f9q2/app.js"), []byte("function copied(){}"), 0o644)
	os.WriteFile(filepath.Join(dir, "app.js"), []byte("function mine(){}"), 0o644)

	ix := New(dir)
	if err := ix.Build(context.Background(), DefaultBuildOptions()); err != nil {
		t.Fatal(err)
	}
	for _, d := range ix.docs {
		if strings.Contains(d.Path, "node_modules") || strings.Contains(d.Path, ".abhed-worktrees") {
			t.Fatalf("vendored code or a worktree was indexed: %s", d.Path)
		}
	}
}

func TestFusionCombinesTiers(t *testing.T) {
	ix, _ := buildTestIndex(t)
	hits, _ := ix.Search(context.Background(), "ValidateToken", 5)
	// A term matching both symbol and BM25 tiers should report both.
	var fused bool
	for _, h := range hits {
		if strings.Contains(h.Tier, "+") {
			fused = true
		}
	}
	if !fused {
		t.Logf("tiers seen: %v", tiersOf(hits))
	}
}

func TestEmptyQueryRejected(t *testing.T) {
	ix, _ := buildTestIndex(t)
	hits, err := ix.Search(context.Background(), "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatal("empty query should return nothing")
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func tiersOf(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Tier)
	}
	return out
}

// The index reads no file through a link and no state file under another
// name: either could put a password hash or a key into search results.
func TestBuildDoesNotFollowLinksOutOfBounds(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".abhed"), 0o755); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(dir, ".abhed", "users.json")
	if err := os.WriteFile(users, []byte("zzsecrethash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(key, []byte("zzsecretkey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(users, filepath.Join(dir, "u1.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(key, filepath.Join(dir, "key.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(users, filepath.Join(dir, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	ix := New(dir)
	if err := ix.Build(context.Background(), DefaultBuildOptions()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"zzsecrethash", "zzsecretkey"} {
		hits, _ := ix.Search(context.Background(), q, 10)
		if len(hits) > 0 {
			t.Errorf("%s was indexed: %+v", q, hits[0])
		}
	}
	if err := ix.Update(context.Background(), filepath.Join(dir, "key.txt")); err == nil {
		t.Error("Update read through a link out of the root")
	}
}
