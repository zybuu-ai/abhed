package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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

// endpointWith runs the endpoint questions on the given answers, against a
// closed port, and returns what they chose and what they printed.
func endpointWith(t *testing.T, answers string) (config.ProviderConfig, string) {
	t.Helper()
	var out strings.Builder
	o := onboarding{in: strings.NewReader(answers), out: &out, ctx: context.Background()}
	_, p, err := o.endpoint()
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	return p, out.String()
}

// A pasted key is never taken as a variable name, whatever its shape.
func TestEndpointRefusesAPastedKey(t *testing.T) {
	t.Setenv("ABHED_TEST_UNSET_VAR", "")
	for _, key := range []string{
		"hf_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789",
		"gsk_1234567890abcdefghijklmnopqrstuvwxyz",
		"AIzaSyA1b2C3d4E5f6G7h8I9j0KlMnOpQrStUvW",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github_pat_11ABCDEFG0123456789_abcdefghij",
		"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6",
		"Zx81kPq2Lm9Rt4Vb7Nc3Hd6Jf0Gs5Wa1",
	} {
		t.Run(key[:4], func(t *testing.T) {
			p, out := endpointWith(t, "http://127.0.0.1:9/v1\n"+key+"\n\nm\n")
			if p.APIKeyEnv != "" || strings.Contains(out, key) {
				t.Fatalf("the key was taken: %q\n%s", p.APIKeyEnv, out)
			}
			if !strings.Contains(out, "looks like a key") {
				t.Fatalf("no refusal:\n%s", out)
			}
		})
	}
}

// A variable that is not set is taken only on a yes; no answer is a no.
func TestEndpointAsksAboutAnUnsetVariable(t *testing.T) {
	t.Setenv("ABHED_TEST_UNSET_VAR", "")
	if p, out := endpointWith(t, "http://127.0.0.1:9/v1\nABHED_TEST_UNSET_VAR\n\n\nm\n"); p.APIKeyEnv != "" {
		t.Fatalf("taken without a yes:\n%s", out)
	}
	if p, out := endpointWith(t, "http://127.0.0.1:9/v1\nABHED_TEST_UNSET_VAR\ny\nm\n"); p.APIKeyEnv != "ABHED_TEST_UNSET_VAR" {
		t.Fatalf("not taken after a yes:\n%s", out)
	}
	t.Setenv("ABHED_TEST_SET_VAR", "value")
	if p, out := endpointWith(t, "http://127.0.0.1:9/v1\nABHED_TEST_SET_VAR\nm\n"); p.APIKeyEnv != "ABHED_TEST_SET_VAR" {
		t.Fatalf("a set variable was not taken:\n%s", out)
	}
}

// A key is not sent in the clear to another machine without a yes.
func TestEndpointWarnsBeforeAKeyGoesOverHTTP(t *testing.T) {
	t.Setenv("ABHED_TEST_SET_VAR", "value")
	p, out := endpointWith(t, "http://192.0.2.1:9/v1\nABHED_TEST_SET_VAR\n\nhttp://127.0.0.1:9/v1\nABHED_TEST_SET_VAR\nm\n")
	if !strings.Contains(out, "unencrypted") || p.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("%+v\n%s", p, out)
	}
}

func TestLooksLikeKey(t *testing.T) {
	for _, key := range []string{"lsv2_pt_0123abcd", "tvly-abc", "pa-abc", "jina_abc"} {
		if !looksLikeKey(key) {
			t.Errorf("%s was taken for a name", key)
		}
	}
	for _, name := range []string{"OPENAI_API_KEY", "MY_ENDPOINT_KEY", "ABHED_COMPANY_INTERNAL_GATEWAY_API_KEY", "K", "token"} {
		if looksLikeKey(name) {
			t.Errorf("%s was taken for a key", name)
		}
	}
}

// Model names from the endpoint are shown escaped: a hostile endpoint
// cannot drive the terminal through them.
func TestEndpointModelNamesAreEscaped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"good"},{"id":"evil\u001b]0;pwned\u0007\u202e"}]}`)
	}))
	defer srv.Close()
	_, out := endpointWith(t, srv.URL+"\n\ngood\n")
	if strings.ContainsAny(out, "\x1b\x07\u202e") || !strings.Contains(out, `evil\u001b`) {
		t.Fatalf("%q", out)
	}
}
