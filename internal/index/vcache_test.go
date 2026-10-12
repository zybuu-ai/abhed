package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// countingEmbedder returns a fixed vector and counts the texts it embeds.
type countingEmbedder struct{ texts int }

func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.texts += len(texts)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (e *countingEmbedder) Dimensions() int { return 2 }

// A second build with the cache saved by the first embeds only the chunk
// that changed, and every chunk still has a vector.
func TestVectorCacheEmbedsOnlyWhatChanged(t *testing.T) {
	ws := t.TempDir()
	for name, body := range map[string]string{"a.go": "package a\n\nfunc A() {}\n", "b.go": "package b\n\nfunc B() {}\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "vectors.gob")
	opts := DefaultBuildOptions()
	opts.Embed = true
	build := func() (*Index, *countingEmbedder) {
		t.Helper()
		e := &countingEmbedder{}
		c := OpenFileVectorCache(path)
		ix := New(ws).WithEmbedder(e).WithVectorCache(c)
		if err := ix.Build(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
		return ix, e
	}
	ix, first := build()
	docs, _, vectors, _ := ix.Stats()
	if first.texts != docs || vectors != docs || docs == 0 {
		t.Fatalf("first build: %d embedded, %d vectors, %d docs", first.texts, vectors, docs)
	}
	if _, again := build(); again.texts != 0 {
		t.Fatalf("an unchanged workspace embedded %d chunks again", again.texts)
	}
	if err := os.WriteFile(filepath.Join(ws, "b.go"), []byte("package b\n\nfunc B2() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ix, changed := build()
	docs, _, vectors, _ = ix.Stats()
	if changed.texts == 0 || changed.texts >= docs || vectors != docs {
		t.Fatalf("after one change: %d embedded of %d, %d vectors", changed.texts, docs, vectors)
	}
}
