package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/ui"
)

// Until mentions are built, a line goes to the model exactly as typed and
// nothing is read: an @ is text, not a file.
func TestPlainInputSendsTheLineAsTyped(t *testing.T) {
	for _, raw := range []string{"fix it", "look at @~/.ssh/id_rsa", "!rm -rf build", "# note"} {
		msg, att, err := plainInput{}.Expand(context.Background(), nil, raw)
		if err != nil || msg.Text != raw || len(att) != 0 {
			t.Fatalf("%q became %+v, %v, %v", raw, msg, att, err)
		}
	}
	a := Attachment{Path: "a.go", Range: "1-2", SHA256: "ab", Bytes: 9, Truncated: true}
	if a.Mention() != (agent.InputMention{Path: "a.go", Range: "1-2", SHA256: "ab", Bytes: 9, Truncated: true}) {
		t.Fatalf("mention %+v", a.Mention())
	}
}

// scriptSurface answers each Dialog with the next scripted answer ("" is no
// answer) and keeps what it was shown.
type scriptSurface struct {
	answers []string
	asked   []ui.DialogSpec
	blocks  []ui.Block
}

var _ ui.Surface = (*scriptSurface)(nil)

func (s *scriptSurface) Append(b ui.Block) { s.blocks = append(s.blocks, b) }
func (s *scriptSurface) Dialog(_ context.Context, d ui.DialogSpec) (string, error) {
	if _, err := d.Normalized(); err != nil {
		return "", err
	}
	s.asked = append(s.asked, d)
	if len(s.answers) == 0 || s.answers[0] == "" {
		if len(s.answers) > 0 {
			s.answers = s.answers[1:]
		}
		return "", ui.ErrNoAnswer
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}
func (s *scriptSurface) Pick(context.Context, ui.PickSpec) (string, error) { return "", ui.ErrNoAnswer }
func (s *scriptSurface) Panel(_ context.Context, p ui.PanelSpec) error {
	s.blocks = append(s.blocks, p.Body...)
	return nil
}
func (s *scriptSurface) SetStatus(ui.StatusModel) {}
func (s *scriptSurface) Notify(ui.Toast)          {}

// shown is everything appended, as text.
func (s *scriptSurface) shown() string {
	var b strings.Builder
	for _, bl := range s.blocks {
		b.WriteString(bl.Text + "\n")
		for _, r := range bl.Rows {
			b.WriteString(strings.Join(r, " ") + "\n")
		}
	}
	return b.String()
}

// -json-schema @file is read relative to the workspace, bounded, and must
// be an object.
func TestSchemaFlagFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "s.json"), []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := schemaFlag("@s.json", ws); err != nil || string(got) != `{"type":"object"}` {
		t.Fatalf("%s %v", got, err)
	}
	big := append([]byte(`{"description":"`), make([]byte, maxSchema)...)
	if err := os.WriteFile(filepath.Join(ws, "big.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := schemaFlag("@big.json", ws); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("big: %v", err)
	}
	if _, err := os.Stat("/dev/zero"); err == nil {
		if _, err := schemaFlag("@/dev/zero", ws); err == nil {
			t.Fatal("/dev/zero was taken")
		}
	}
	for _, v := range []string{`[1]`, `"s"`, `nope`} {
		if _, err := schemaFlag(v, ws); err == nil {
			t.Errorf("%s was taken", v)
		}
	}
}
