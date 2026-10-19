package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
)

// abhed serve on the memory store writes its sessions to the OCSF file, read
// from the store by polling it.
func TestServeWritesTheOCSFFileFromMemory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "serve.jsonl")
	cfg := config.Config{Record: config.RecordConfig{OCSF: config.OCSFConfig{Enabled: true, Path: path}}}
	st := agent.NewMemStore()
	stop, err := serveOCSF(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	r := agent.NewRecorder(st, "s-serve", "")
	if _, err := r.Record(agent.EvUserMessage, agent.ActorUser, agent.Trusted, agent.Message{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, _ := os.ReadFile(path); strings.Contains(string(data), `"correlation_uid":"s-serve"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	data, _ := os.ReadFile(path)
	if evs := ocsfLines(t, string(data)); len(evs) != 1 || evs[0].SessionID != "s-serve" {
		t.Fatalf("the file holds %s", data)
	}
}

func TestServeOCSFOffWritesNothing(t *testing.T) {
	stop, err := serveOCSF(config.Config{}, agent.NewMemStore())
	if err != nil {
		t.Fatal(err)
	}
	stop()
}
