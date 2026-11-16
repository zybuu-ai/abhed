package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recordingEmbedder keeps every text it is asked to embed.
type recordingEmbedder struct{ texts []string }

func (r *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	r.texts = append(r.texts, texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// A file the read rules withhold is never indexed: neither searched nor
// sent to the embedding endpoint, and Update refuses it too.
func TestBuildSkipsWithheldFiles(t *testing.T) {
	dir := t.TempDir()
	for rel, content := range map[string]string{
		"app/config.go":     "package app\n\n// apiKeyName names the API key setting.\nconst apiKeyName = \"API_KEY\"\n",
		"secrets/keys.yaml": "api_key: s3cr3t-INDEXLEAK\n",
	} {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	emb := &recordingEmbedder{}
	ix := New(dir).WithEmbedder(emb)
	opts := DefaultBuildOptions()
	opts.Embed = true
	opts.Withheld = func(p string) bool {
		return strings.Contains(p, string(filepath.Separator)+"secrets"+string(filepath.Separator))
	}
	if err := ix.Build(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Search(context.Background(), "api_key", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if strings.Contains(h.Doc.Content, "INDEXLEAK") {
			t.Fatal("a withheld file was indexed")
		}
	}
	for _, s := range emb.texts {
		if strings.Contains(s, "INDEXLEAK") {
			t.Fatal("a withheld file was sent to the embedding endpoint")
		}
	}
	if len(emb.texts) == 0 {
		t.Fatal("the fixture embedded nothing")
	}
	if err := ix.Update(context.Background(), filepath.Join(dir, "secrets", "keys.yaml")); err == nil {
		t.Fatal("Update indexed a withheld file")
	}
}

func (*recordingEmbedder) Dimensions() int { return 2 }
