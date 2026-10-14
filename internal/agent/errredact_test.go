package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/policy"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// echoErrAdapter fails every call with an error that echoes a value.
type echoErrAdapter struct {
	scriptedAdapter
	err error
}

func (f *echoErrAdapter) Complete(context.Context, model.Request) (<-chan model.Chunk, error) {
	return nil, f.err
}

// An error a run returns, such as a provider's body echoing a key, has
// stored values taken out, and its cause still answers errors.As.
func TestRunErrorIsRedacted(t *testing.T) {
	dir := t.TempDir()
	vault := secrets.Open(filepath.Join(t.TempDir(), "secrets.json"))
	if err := vault.Set("API_KEY", "sk-live-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	sess, _ := tools.NewSession(dir)
	rec := NewRecorder(NewMemStore(), "s1", "")
	rec.Redact = vault.Redactor()
	cause := &model.StatusError{Status: 401, Body: "invalid key sk-live-0123456789abcdef", Attempts: 1}
	l := NewLoop(&echoErrAdapter{err: cause}, tools.NewRegistry(), policy.New(policy.ModeDefault),
		AutoApprove{Yes: true}, sess, rec, DefaultConfig())
	_, err := l.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("the run did not fail")
	}
	if strings.Contains(err.Error(), "sk-live") || !strings.Contains(err.Error(), "[secret:API_KEY]") {
		t.Fatalf("the error was not redacted: %v", err)
	}
	var se *model.StatusError
	if !errors.As(err, &se) || se.Status != 401 {
		t.Fatalf("the cause was lost: %v", err)
	}
}
