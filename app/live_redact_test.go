package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// storeSecrets replaces the store's contents as `abhed secret set` would, by rename.
func storeSecrets(t *testing.T, path string, m map[string]string) {
	t.Helper()
	raw, _ := json.Marshal(m)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Error(err)
	}
}

// The interactive CLI redacts a secret stored, or changed, after it started:
// bash is handed the value at the call, so the record and the model must not see it.
func TestTerminalRedactsASecretStoredMidSession(t *testing.T) {
	const first, second = "fake-late-secret-one-41c7e2", "fake-late-secret-two-9a03bd"
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".abhed"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".abhed", "secrets.json")
	storeSecrets(t, path, map[string]string{"OTHER_TOKEN": "fake-other-secret-000000"})

	args, _ := json.Marshal(map[string]any{"command": `echo "k=[$LATE_TOKEN]"`,
		"description": "probe", "secrets": []string{"LATE_TOKEN"}})
	call, _ := json.Marshal(string(args))
	reply := func(w io.Writer, n int, _ string) {
		switch n {
		case 1, 3:
			// Stored after the session started: first added, then changed.
			value := first
			if n == 3 {
				value = second
			}
			storeSecrets(t, path, map[string]string{"OTHER_TOKEN": "fake-other-secret-000000", "LATE_TOKEN": value})
			fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c`+fmt.Sprint(n)+`","function":{"name":"bash","arguments":`+string(call)+`}}]}}]}`)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		default:
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"noted %d\"}}]}\n\n", n)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}
	c := startCLIEnv(t, reply, func(url string) string {
		return `{"model":{"default":"stub","providers":{"stub":{"type":"openai-compatible","base_url":"` + url +
			`","model":"m","context_window":8192}}},"permissions":{"allow":["bash","secret(LATE_*)"]}}`
	}, "ABHED_CONV_HOME="+home)

	c.task("probe one")
	c.task("probe two")
	c.task("anything else")

	raw, _ := json.Marshal(c.export())
	c.mu.Lock()
	sent := strings.Join(c.bodies, "\n")
	c.mu.Unlock()
	for _, v := range []string{first, second} {
		if strings.Contains(string(raw), v) {
			t.Errorf("the record holds %s", v)
		}
		if strings.Contains(sent, v) {
			t.Errorf("a model request holds %s", v)
		}
	}
	if strings.Count(sent, "k=[[secret:LATE_TOKEN]]") < 2 {
		t.Errorf("bash was not handed the late secret both times:\n%s", c.out.String())
	}
}
