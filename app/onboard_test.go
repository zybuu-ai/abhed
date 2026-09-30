package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/model"
)

func TestFriendlyModelError(t *testing.T) {
	p := config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://127.0.0.1:9/v1", Model: "m", APIKeyEnv: "ABHED_TEST_UNSET_KEY"}
	refused := fmt.Errorf("Post \"http://127.0.0.1:9/v1/chat/completions\": %w",
		&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
	for _, c := range []struct {
		err       error
		want, not string
	}{
		{refused, "Nothing is answering at http://127.0.0.1:9/v1", "dial tcp"},
		{&net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}, "does not resolve", "no such host"},
		{&model.StatusError{Status: 401, Body: `{"error":"bad key sk-123"}`, Attempts: 1}, "$ABHED_TEST_UNSET_KEY, which is not set", "sk-123"},
		{&model.StatusError{Status: 404, Body: `{"detail":"x"}`}, "has no such model or path", "detail"},
		{&model.StatusError{Status: 503, Body: `upstream`, Attempts: 4}, "after 4 attempts", "upstream"},
		{errors.New("something else"), "something else", "abhed doctor"},
	} {
		got := friendlyModelError(c.err, "local", p)
		if !strings.Contains(got, c.want) || strings.Contains(got, c.not) {
			t.Errorf("%v:\n%s", c.err, got)
		}
	}
}

func TestDialAddr(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:11434/v1": "localhost:11434", "https://api.example.com/v1": "api.example.com:443",
		"http://h/v1": "h:80", "": "", "not a url": "",
	} {
		if got := dialAddr(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

// A probe that found the endpoint down fails the next call at once; one
// that found it up lets calls through.
func TestEndpointProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	up := &endpointProbe{addr: addr, first: make(chan struct{})}
	if err := up.run(context.Background()); err != nil || up.check(context.Background()) != nil {
		t.Fatalf("an open port was reported down: %v", err)
	}
	_ = ln.Close()
	down := &endpointProbe{addr: addr, first: make(chan struct{})}
	if err := down.run(context.Background()); err == nil {
		t.Skip("the closed port still accepted a connection")
	}
	if err := down.check(context.Background()); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("check: %v", err)
	}
	started := &endpointProbe{addr: addr, first: make(chan struct{})}
	started.start(context.Background())
	if err := started.firstResult(5 * time.Second); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("first result: %v", err)
	}
}

func TestFirstRunSkipWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:9")
	var out strings.Builder
	if err := firstRun(context.Background(), strings.NewReader("s\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not running") || !strings.Contains(out.String(), "Nothing written") {
		t.Fatalf("%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".abhed", "config.json")); err == nil {
		t.Fatal("skip wrote a config")
	}
	// Input ending mid-way is an error, and writes nothing either.
	if err := firstRun(context.Background(), strings.NewReader(""), &out); err == nil {
		t.Fatal("no answer was taken as one")
	}
}

func TestWriteUserConfigNeverOverwritesOrStoresAKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := config.ProviderConfig{Type: "openai-compatible", BaseURL: "http://x/v1", Model: "m", APIKey: "sk-secret", APIKeyEnv: "K"}
	path, err := writeUserConfig("endpoint", p, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "sk-secret") || !strings.Contains(string(data), `"api_key_env": "K"`) {
		t.Fatalf("%s", data)
	}
	if _, err := writeUserConfig("endpoint", p, false); err == nil {
		t.Fatal("overwrote an existing config")
	}
}

func TestOllamaBase(t *testing.T) {
	for in, want := range map[string]string{"": "http://localhost:11434/v1", "10.0.0.2:11434": "http://10.0.0.2:11434/v1",
		"http://h:1/v1": "http://h:1/v1", "https://h/": "https://h/v1"} {
		t.Setenv("OLLAMA_HOST", in)
		if got := ollamaBase(); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}
