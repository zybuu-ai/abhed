package index

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A file left out by keep changes nothing a search finds, nor its order:
// the answer is the one an index without that file gives, whatever it holds.
func TestSearchKeptIsAsIfTheFileWereNotIndexed(t *testing.T) {
	build := func(withSecret string) *Index {
		dir := t.TempDir()
		files := map[string]string{
			"a.txt": "alpha alpha\n",
			"b.txt": "beta\n",
			"c.txt": "gamma\n",
			"d.txt": "delta\n",
		}
		if withSecret != "" {
			files["secrets/keys.yaml"] = withSecret
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
		return ix
	}
	keep := func(p string) bool { return !strings.Contains(p, "/secrets/") }
	order := func(ix *Index, q string) []string {
		hits, err := ix.SearchKept(context.Background(), q, 10, keep)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, filepath.Base(h.Doc.Path))
		}
		return out
	}
	for _, q := range []string{"alpha beta", "beta", "gamma alpha"} {
		a := order(build("alpha\n"), q)
		b := order(build("nothing to see\n"), q)
		c := order(build(""), q)
		if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(a, c) {
			t.Fatalf("%q: the left-out file changes the answer: %v / %v / %v", q, a, b, c)
		}
	}
}
