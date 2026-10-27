package app

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
)

// visionRig is a Studio rig whose model reads images when vision is set.
func visionRig(t *testing.T, vision bool) *studioRig {
	t.Helper()
	m := newScriptModel(t)
	extra := ""
	if vision {
		extra = `,"extra":{"vision":"true"}`
	}
	acpModelsEnv(t, `{"model":{"default":"m","providers":{"m":`+stubProvider(m.URL, "m-1", extra)+`}}}`)
	ws := t.TempDir()
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real
	}
	r := &studioRig{t: t, ws: ws, home: os.Getenv("HOME"), model: m}
	r.cl = newStudioClient(t, ws, true)
	r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
	return r
}

func (r *studioRig) promptBlocks(id string, blocks ...map[string]any) rpcMessage {
	r.t.Helper()
	all := append([]any{map[string]any{"type": "text", "text": "look"}}, toAny(blocks)...)
	return r.cl.call("session/prompt", map[string]any{"sessionId": id, "prompt": all})
}

func toAny(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// §3.4: a linked file's lines and a linked folder are read by the engine
// through the policy, recorded as mentions, and reach the model.
func TestStudioPromptReadsLinkedFiles(t *testing.T) {
	r := visionRig(t, false)
	r.write("src/a.go", "line1\nline2\nline3\nline4\n")
	r.write("src/b.go", "package b\n")
	id := r.open()
	res := r.promptBlocks(id,
		map[string]any{"type": "resource_link", "name": "a.go", "uri": "file://" + r.ws + "/src/a.go#L2-L3"},
		map[string]any{"type": "resource_link", "name": "src", "uri": "file://" + r.ws + "/src"})
	if res.Error != nil {
		t.Fatalf("%+v", res.Error)
	}
	r.model.mu.Lock()
	body := strings.Join(r.model.bodies, "\n")
	r.model.mu.Unlock()
	if !strings.Contains(body, "line2") || !strings.Contains(body, "line3") || strings.Contains(body, "line4") || !strings.Contains(body, "b.go") {
		t.Fatalf("the model was not given the linked lines and folder: %.600s", body)
	}
	m, _ := r.recorded(id, agent.EvInputMention)
	if len(m) != 2 || m[0]["range"] != "2-3" {
		t.Fatalf("mentions %v", m)
	}
	// Abhed's state, and a page while web_fetch is off, refuse the prompt.
	r.write(".abhed/notes.md", "x")
	for _, uri := range []string{"file://" + r.ws + "/.abhed/notes.md", "https://example.com/doc"} {
		res := r.promptBlocks(id, map[string]any{"type": "resource_link", "name": "x", "uri": uri})
		if res.Error == nil {
			t.Fatalf("%s was attached", uri)
		}
	}
}

// An image reaches a model that reads images, with only a note in the
// record; a model that does not refuses the prompt plainly.
func TestStudioPromptImages(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake image bytes"))
	img := map[string]any{"type": "image", "mimeType": "image/png", "data": png}

	r := visionRig(t, false)
	id := r.open()
	if res := r.promptBlocks(id, img); res.Error == nil || !strings.Contains(res.Error.Message, "does not read images") {
		t.Fatalf("%+v", res)
	}

	v := visionRig(t, true)
	vid := v.open()
	if res := v.promptBlocks(vid, img); res.Error != nil {
		t.Fatalf("%+v", res.Error)
	}
	v.model.mu.Lock()
	body := strings.Join(v.model.bodies, "\n")
	v.model.mu.Unlock()
	if !strings.Contains(body, "data:image/png;base64,"+png) {
		t.Fatalf("the image did not reach the model: %.400s", body)
	}
	msgs, _ := v.recorded(vid, agent.EvUserMessage)
	if len(msgs) != 1 || strings.Contains(msgs[0]["text"].(string), png) || !strings.Contains(msgs[0]["text"].(string), "attached an image") {
		t.Fatalf("user.message %v", msgs)
	}
	m, _ := v.recorded(vid, agent.EvInputMention)
	if len(m) != 1 || m[0]["path"] != "image:image/png" {
		t.Fatalf("mentions %v", m)
	}
}
