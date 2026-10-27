package app

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/managed"
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
	// Abhed's state refuses the prompt.
	r.write(".abhed/notes.md", "x")
	for _, uri := range []string{"file://" + r.ws + "/.abhed/notes.md"} {
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

// A prompt's images are bounded in all, and a message over a line's limit is
// refused with its id while the connection, and its sessions, go on.
func TestStudioOversizedPromptsAreRefused(t *testing.T) {
	v := visionRig(t, true)
	id := v.open()
	big := base64.StdEncoding.EncodeToString(make([]byte, 3500<<10))
	img := map[string]any{"type": "image", "mimeType": "image/png", "data": big}
	if res := v.promptBlocks(id, img, img, img); res.Error == nil || !strings.Contains(res.Error.Message, "in all") {
		t.Fatalf("%+v", res.Error)
	}
	huge := strings.Repeat("x", acpLineMax+10)
	from := v.cl.mark()
	v.cl.write(rpcMessage{JSONRPC: "2.0", ID: json.RawMessage(`"big"`), Method: "session/prompt",
		Params: json.RawMessage(`{"sessionId":"` + id + `","prompt":[{"type":"text","text":"` + huge + `"}]}`)})
	got := v.cl.waitFor(from, "the refusal", func(m rpcMessage) bool { return string(m.ID) == `"big"` })
	if got.Error == nil || !strings.Contains(got.Error.Message, "MiB a line may hold") {
		t.Fatalf("%+v", got)
	}
	// The connection still answers, and the session still runs a prompt.
	if res := v.promptBlocks(id); res.Error != nil {
		t.Fatalf("after the refusal: %+v", res.Error)
	}
}

// A file: link outside the workspace, through a link that leads out, or
// under a read-deny rule refuses the prompt.
func TestStudioLinkedFilesStayInBounds(t *testing.T) {
	m := newScriptModel(t)
	acpModelsEnv(t, `{"model":{"default":"m","providers":{"m":`+stubProvider(m.URL, "m-1")+`}},"permissions":{"deny":["read(**/hidden/**)"]}}`)
	ws := t.TempDir()
	if real, err := filepath.EvalSymlinks(ws); err == nil {
		ws = real
	}
	r := &studioRig{t: t, ws: ws, home: os.Getenv("HOME"), model: m}
	r.cl = newStudioClient(t, ws, true)
	r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "o.txt"), []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "o.txt"), filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r.write("hidden/p.txt", "PRIVATE")
	id := r.open()
	for _, uri := range []string{"file:///etc/hosts", "file://" + ws + "/link.txt", "file://" + ws + "/hidden/p.txt"} {
		res := r.promptBlocks(id, map[string]any{"type": "resource_link", "name": "x", "uri": uri})
		if res.Error == nil {
			t.Fatalf("%s was attached", uri)
		}
	}
	r.model.mu.Lock()
	body := strings.Join(r.model.bodies, "\n")
	r.model.mu.Unlock()
	if strings.Contains(body, "OUTSIDE") || strings.Contains(body, "PRIVATE") {
		t.Fatal("refused content reached the model")
	}
}

// A prompt that is a slash command carries its images to the command's run,
// and an image given with a command that runs no prompt never reaches a
// later one.
func TestStudioImagesGoWithTheCommandAndNoFurther(t *testing.T) {
	v := visionRig(t, true)
	if err := os.MkdirAll(filepath.Join(v.home, ".abhed", "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.home, ".abhed", "commands", "look.md"), []byte("Look at $ARGUMENTS"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := v.open()
	img := func(b string) map[string]any {
		return map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString([]byte(b))}
	}
	prompt := func(text string, blocks ...map[string]any) {
		all := append([]any{map[string]any{"type": "text", "text": text}}, toAny(blocks)...)
		if res := v.cl.call("session/prompt", map[string]any{"sessionId": id, "prompt": all}); res.Error != nil {
			t.Fatalf("%s: %+v", text, res.Error)
		}
	}
	last := func() string {
		v.model.mu.Lock()
		defer v.model.mu.Unlock()
		return v.model.bodies[len(v.model.bodies)-1]
	}
	command := base64.StdEncoding.EncodeToString([]byte("command image"))
	prompt("/look this", img("command image"))
	if !strings.Contains(last(), command) {
		t.Fatal("the command's run did not carry the image")
	}
	builtin := base64.StdEncoding.EncodeToString([]byte("builtin image"))
	prompt("/diff", img("builtin image"))
	prompt("plain")
	if strings.Contains(last(), builtin) {
		t.Fatal("an image given with a built-in command reached a later prompt")
	}
	if strings.Count(last(), command) != 1 {
		t.Fatalf("the command's image was attached again: %d", strings.Count(last(), command))
	}
}

// Whether images may be sent is said per model, in the model list Studio
// reads as the model changes; promptCapabilities.image is false.
func TestStudioSaysWhichModelsReadImages(t *testing.T) {
	for _, vision := range []bool{false, true} {
		v := visionRig(t, vision)
		var res struct {
			ConfigOptions []struct {
				Options []struct {
					Meta map[string]struct {
						Image bool `json:"image"`
					} `json:"_meta"`
				} `json:"options"`
			} `json:"configOptions"`
		}
		v.cl.ok("session/new", map[string]any{"cwd": v.ws, "mcpServers": []any{}}, &res)
		if len(res.ConfigOptions) == 0 || len(res.ConfigOptions[0].Options) != 1 || res.ConfigOptions[0].Options[0].Meta[acpMetaKey].Image != vision {
			t.Fatalf("vision %v: %+v", vision, res)
		}
	}
	m := newScriptModel(t)
	acpModelsEnv(t, `{"model":{"default":"m","providers":{"m":`+stubProvider(m.URL, "m-1")+`}}}`)
	cl := newStudioClient(t, t.TempDir(), true)
	var init struct {
		AgentCapabilities struct {
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	cl.ok("initialize", map[string]any{"protocolVersion": 1}, &init)
	if init.AgentCapabilities.PromptCapabilities.Image {
		t.Fatal("promptCapabilities.image is true")
	}
}

// A linked page is read as the person's own call even while the agent's
// web_fetch is off: deny rules still refuse it, an ask rule puts it to the
// person, internal addresses stay refused, and only a managed configuration
// that turns web_fetch off refuses it outright.
func TestStudioLinkedPageIsThePersonsFetch(t *testing.T) {
	m := newScriptModel(t)
	acpModelsEnv(t, `{"model":{"default":"m","providers":{"m":`+stubProvider(m.URL, "m-1")+`}},`+
		`"permissions":{"deny":["web_fetch(https://blocked.example/*)"],"ask":["web_fetch(https://ask.example/*)"]}}`)
	ws := t.TempDir()
	r := &studioRig{t: t, ws: ws, home: os.Getenv("HOME"), model: m}
	r.cl = newStudioClient(t, ws, true)
	r.cl.ok("initialize", map[string]any{"protocolVersion": 1}, nil)
	var asked atomic.Int32
	var lastAsk atomic.Value
	r.cl.answering(func(method string, params json.RawMessage) any {
		if method == "session/request_permission" {
			asked.Add(1)
			lastAsk.Store(string(params))
			return chosen(params, "reject_once")
		}
		return map[string]any{}
	})
	id := r.open()
	link := func(uri string) string {
		res := r.promptBlocks(id, map[string]any{"type": "resource_link", "name": "page", "uri": uri})
		if res.Error == nil {
			t.Fatalf("%s was read", uri)
		}
		return res.Error.Message
	}
	if why := link("https://blocked.example/doc"); !strings.Contains(why, "Denied") || asked.Load() != 0 {
		t.Fatalf("deny: %s (asked %d)", why, asked.Load())
	}
	if why := link("https://ask.example/doc"); asked.Load() != 1 || strings.Contains(why, "web_fetch, which is off") {
		t.Fatalf("ask: %s (asked %d)", why, asked.Load())
	}
	// An internal address is refused by the address check itself, before anyone is asked.
	if why := link("http://127.0.0.1:1/x"); !strings.Contains(why, "which web_fetch never reaches") || strings.Contains(why, "secrets store") || asked.Load() != 1 {
		t.Fatalf("internal: %s (asked %d)", why, asked.Load())
	}
	// A public page passes every check (the secrets store read, the address
	// check) and, with no allowed hosts, is put to the person, who refuses it here.
	if why := link("https://public.example/doc"); asked.Load() != 2 || strings.Contains(why, "secrets store") {
		t.Fatalf("public: %s (asked %d)", why, asked.Load())
	}
	if ask, _ := lastAsk.Load().(string); !strings.Contains(ask, `"tool":"web_fetch"`) || !strings.Contains(ask, `"kind":"fetch"`) || !strings.Contains(ask, `"url":"https://public.example/doc"`) {
		t.Fatalf("the fetch was not shown as a fetch: %s", ask)
	}

	// A managed configuration that turns web_fetch off refuses it plainly.
	path := filepath.Join(t.TempDir(), "managed.json")
	if err := os.WriteFile(path, []byte(`{"web_fetch":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	managed.ConfigFile = path // acpModelsEnv restores it
	id = r.open()
	if why := link("https://example.com/doc"); !strings.Contains(why, "managed configuration turns web_fetch off") {
		t.Fatalf("managed: %s", why)
	}
}
