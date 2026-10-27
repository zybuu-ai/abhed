package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/toolset"
	abhed "github.com/zybuu-ai/abhed/sdk"
)

// What a prompt links or carries beyond its text (studio-acp-contract.md
// §3.4): a linked file, range or folder is read by the engine as an @ mention
// is, through the session's policy and recorded as the person's; a linked web
// page goes through web_fetch as the person's call, when web_fetch is on; an
// image goes to a model that reads images, held in memory only.

// Image bounds: how many a prompt may carry, how big each may be, and how
// big all of them, which keeps a prompt's base64 well under a line's 16 MiB.
const (
	promptImages     = 4
	promptImageBytes = 5 << 20
	promptImageTotal = 10 << 20
)

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// promptLinks is what a prompt's links and images add: text for the model,
// the images, the mentions to record, and why the prompt is refused instead.
type promptLinks struct {
	text     string
	images   []abhed.Image
	mentions []agent.InputMention
	why      string
}

// linkedAttachments reads a prompt's resource_link and image blocks. room is
// what the prompt's embedded resources left of the shared attachment cap.
func (c *acpConn) linkedAttachments(ctx context.Context, s *acpSession, blocks []json.RawMessage, room int) (promptLinks, error) {
	var out promptLinks
	if !s.inner || s.parts.Loop == nil || s.parts.Session == nil {
		// Not read here: a link is named for the agent, and an image refuses the prompt.
		var notes []string
		for _, raw := range blocks {
			var blk struct {
				Type string `json:"type"`
				URI  string `json:"uri"`
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &blk) != nil {
				continue
			}
			switch blk.Type {
			case "image":
				out.why = "this session cannot take images"
				return out, nil
			case "resource_link":
				notes = append(notes, fmt.Sprintf("(See %s at %s)", blk.Name, blk.URI))
			}
		}
		if len(notes) > 0 {
			out.text = "\n\n" + strings.Join(notes, "\n")
		}
		return out, nil
	}
	loop := s.parts.Loop
	var parts, refused []string
	imageBytes := 0
	for _, raw := range blocks {
		var blk struct {
			Type     string `json:"type"`
			URI      string `json:"uri"`
			Name     string `json:"name"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		}
		if json.Unmarshal(raw, &blk) != nil {
			continue
		}
		switch blk.Type {
		case "image":
			if len(out.images) == promptImages {
				refused = append(refused, fmt.Sprintf("a prompt carries at most %d images", promptImages))
				continue
			}
			if !loop.Adapter.Profile().SupportsVision {
				refused = append(refused, "the model "+loop.Adapter.Profile().Name+" does not read images; choose one that does, or describe the image")
				continue
			}
			if !imageTypes[blk.MimeType] {
				refused = append(refused, fmt.Sprintf("an image of type %q cannot be sent (png, jpeg, gif or webp)", blk.MimeType))
				continue
			}
			// Its size is known before it is decoded, so an oversized one costs nothing.
			// The decoder skips line breaks, so they are not counted as data.
			wrapped := strings.Count(blk.Data, "\n") + strings.Count(blk.Data, "\r")
			if n := base64.StdEncoding.DecodedLen(len(blk.Data)-wrapped) - 2; n > promptImageBytes || imageBytes+n > promptImageTotal {
				refused = append(refused, fmt.Sprintf("an image may hold %d MB, and a prompt's images %d MB in all", promptImageBytes>>20, promptImageTotal>>20))
				continue
			}
			data, err := base64.StdEncoding.DecodeString(blk.Data)
			if err != nil || len(data) == 0 {
				refused = append(refused, "an image's data is not base64")
				continue
			}
			if len(data) > promptImageBytes {
				refused = append(refused, fmt.Sprintf("an image of %d MB is over the %d MB limit", len(data)>>20, promptImageBytes>>20))
				continue
			}
			if imageBytes+len(data) > promptImageTotal {
				refused = append(refused, fmt.Sprintf("a prompt's images may hold %d MB in all", promptImageTotal>>20))
				continue
			}
			imageBytes += len(data)
			sum := sha256.Sum256(data)
			hash := hex.EncodeToString(sum[:])
			out.images = append(out.images, abhed.Image{MediaType: blk.MimeType, Data: data})
			out.mentions = append(out.mentions, agent.InputMention{Path: "image:" + blk.MimeType, SHA256: hash, Bytes: int64(len(data))})
			parts = append(parts, fmt.Sprintf("[The person attached an image (%s, %d KB, sha256 %s); the model sees it with this message, and the record keeps only this note.]",
				blk.MimeType, (len(data)+1023)>>10, hash[:12]))
		case "resource_link":
			u, err := url.Parse(blk.URI)
			if err != nil {
				continue
			}
			switch strings.ToLower(u.Scheme) {
			case "file":
				block, m, why, err := c.attachLinkedFile(ctx, s, u, room)
				if err != nil {
					return out, err
				}
				if why != "" {
					refused = append(refused, blk.URI+": "+why)
					continue
				}
				room = max(0, room-int(m.Bytes))
				parts = append(parts, block)
				out.mentions = append(out.mentions, m)
			case "http", "https":
				content, why, err := c.fetchLinked(ctx, s, blk.URI)
				if err != nil {
					return out, err
				}
				if why != "" {
					refused = append(refused, blk.URI+": "+why)
					continue
				}
				content, truncated := capText(content, room)
				room = max(0, room-len(content))
				sum := sha256.Sum256([]byte(content))
				out.mentions = append(out.mentions, agent.InputMention{Path: redacted(blk.URI), SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), Truncated: truncated})
				parts = append(parts, fenced("page", fmt.Sprintf("url=%q", redacted(blk.URI)), content))
			default:
				parts = append(parts, fmt.Sprintf("(See %s at %s)", blk.Name, blk.URI))
			}
		}
	}
	if len(refused) > 0 {
		out.why = strings.Join(refused, "\n")
		return out, nil
	}
	if len(parts) > 0 {
		out.text = "\n\nWhat the person linked, read through the session's policy. " + untrustedNote + "\n" + strings.Join(parts, "\n")
	}
	return out, nil
}

// attachLinkedFile reads a file:// link, a file with an optional #Lm-Ln
// range or a folder, as an @ mention is read, within room bytes.
func (c *acpConn) attachLinkedFile(ctx context.Context, s *acpSession, u *url.URL, room int) (string, agent.InputMention, string, error) {
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", agent.InputMention{}, "the file is on another host", nil
	}
	if room <= 0 {
		return "", agent.InputMention{}, fmt.Sprintf("the prompt's attachments are over the %d KB cap", mentionMaxBytes>>10), nil
	}
	loop, sess := s.parts.Loop, s.parts.Session
	abs := filepath.Clean(filepath.FromSlash(u.Path))
	ref := mentionRef{Raw: u.Path, Path: abs}
	if m := lineFragment.FindStringSubmatch(u.Fragment); m != nil {
		ref.From, _ = strconv.Atoi(m[1])
		ref.To, _ = strconv.Atoi(m[2])
		if ref.From < 1 || ref.To < ref.From {
			ref.From, ref.To = 0, 0
		}
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return "", agent.InputMention{}, "no such file or folder", nil //nolint:nilerr // a missing link is a refusal, not a failure
	}
	var err error
	var block string
	var att Attachment
	var why string
	if info.IsDir() {
		block, att, why, err = attachDirWithin(ctx, loop, sess, abs, min(room, mentionMaxBytes))
	} else {
		block, att, why, err = attachFileWithin(ctx, loop, sess, abs, ref, min(room, mentionMaxBytes))
	}
	if err != nil || why != "" {
		return "", agent.InputMention{}, why, err
	}
	return block, att.Mention(), "", nil
}

// fetchLinked reads a page the person linked, as their own call: with the
// agent's web_fetch when it has one, else with one made for this read alone,
// under the same policy, deny rules, egress and address checks; an ask is put
// to the person. Only a managed configuration that turns web_fetch off
// refuses it outright.
func (c *acpConn) fetchLinked(ctx context.Context, s *acpSession, uri string) (string, string, error) {
	loop, cfg := s.parts.Loop, s.parts.Config
	var tool tools.Tool
	if t, ok := loop.Tools.Get("web_fetch"); ok {
		tool = t
	} else {
		if cfg.ManagedSets("web_fetch.enabled") {
			return "", "the organisation's managed configuration turns web_fetch off, so a linked page cannot be read", nil
		}
		on := cfg
		on.WebFetch.Enabled = true
		f := toolset.WebFetchTool(on, nil)
		f.Guard = s.parts.Set.Guard()
		tool = f
	}
	id := personCallID("link")
	res, err := loop.ManualRunTool(ctx, s.parts.Session, tool, id, argsJSON(map[string]string{"url": uri}),
		c.manualAsk(s, id, "fetch", "Read the linked page "+redacted(uri)))
	if err != nil {
		return "", "", err
	}
	if res.IsError {
		return "", strings.TrimSpace(res.Content), nil
	}
	return res.Content, "", nil
}
