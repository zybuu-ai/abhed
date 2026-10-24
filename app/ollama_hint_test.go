package app

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// A headless run whose local Ollama model is down says what is wrong, the
// steps, and that abhed ollama check offers them.
func TestFriendlyModelErrorForAnOllamaModel(t *testing.T) {
	f := newAppOllama(t)
	refused := fmt.Errorf("Post %q: %w", f.url+"/v1/chat/completions",
		&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)})
	got := friendlyModelError(refused, "olm", f.provider())
	for _, want := range []string{"installed but not running", "1. OLLAMA_HOST=127.0.0.1:", "ollama pull qwen3:8b", "abhed ollama check olm"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if len(f.launched) != 0 {
		t.Fatal("explaining the error started Ollama")
	}
}
