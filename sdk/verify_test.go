package abhed_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	abhed "github.com/zybuu-ai/abhed/sdk"
)

// With Verify strict, an answer the record cannot back ends the run with an
// EndedError whose Reason is unverified, after one revision, and Verdict
// reports what was found.
func TestVerifyStrictThroughTheSDK(t *testing.T) {
	t.Setenv("ABHED_TRUST_WORKSPACE", "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ABHED_SECRETS_FILE", "")
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"I updated deploy.yaml and all 9 checks pass.\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	cfg := `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + srv.URL + `","model":"m","context_window":8192}}}}`
	os.MkdirAll(filepath.Join(ws, ".abhed"), 0o700)
	if err := os.WriteFile(filepath.Join(ws, ".abhed", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := abhed.New(context.Background(), abhed.Options{Workspace: ws, ConfigDir: ws, Verify: "sometimes"}); err == nil {
		t.Fatal("an unknown Verify is refused")
	}
	a, err := abhed.New(context.Background(), abhed.Options{Workspace: ws, ConfigDir: ws, Verify: "strict"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, err = a.Run(context.Background(), "deploy it")
	var ended *abhed.EndedError
	if !errors.As(err, &ended) || ended.Reason != "unverified" {
		t.Fatalf("want an unverified end, got %v", err)
	}
	v := a.Verdict()
	if v == nil || v.Outcome != "unverified" || v.Unverified == 0 || v.Contradicted != 0 {
		t.Fatalf("verdict %+v", v)
	}
}

// A verdict's parts are named in the SDK, so a caller outside the module
// can walk them.
func TestVerdictTypesAreExported(t *testing.T) {
	v := abhed.AnswerVerdict{Claims: []abhed.ClaimVerdict{{Conflict: &abhed.Conflict{Seq: 3}}},
		Contract: &abhed.ContractResult{Require: []abhed.RequireResult{{Command: "go test ./..."}}}}
	if v.Claims[0].Conflict.Seq != 3 || v.Contract.Require[0].Command == "" {
		t.Fatal("unreachable")
	}
}
