package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// resourceMentionRe is an @server:uri mention: a server name, then a URI
// with a scheme of its own, as @docs:file:///guide.md or @tickets:ticket://42.
var resourceMentionRe = regexp.MustCompile(`^([A-Za-z0-9_-]{1,64}):(.+)$`)

// resourceMention splits an @server:uri mention, and names the session's
// tool that reads it; ok is false for text that is not one, left as typed.
func resourceMention(loop *agent.Loop, raw string) (server, uri, tool string, ok bool) {
	m := resourceMentionRe.FindStringSubmatch(raw)
	if m == nil || !mcp.ValidResourceURI(m[2]) || !strings.Contains(m[2], ":") {
		return "", "", "", false
	}
	tool = "mcp__" + m[1] + "__" + mcp.ReadResourceTool
	if loop == nil || loop.Tools == nil {
		return "", "", "", false
	}
	if _, found := loop.Tools.Get(tool); !found {
		return "", "", "", false
	}
	return m[1], m[2], tool, true
}

// attachResource reads an @server:uri mention through the server's
// read_resource tool as the person: the session's policy decides, the read
// is recorded, and the text is redacted and capped at limit bytes. why is
// the refusal when it may not be read; err is a failure to record.
func attachResource(ctx context.Context, loop *agent.Loop, sess *tools.Session, server, uri, tool string, limit int) (block string, att Attachment, why string, err error) {
	res, err := loop.Manual(ctx, sess, tool, personCallID("mention"), argsJSON(map[string]string{"uri": uri}))
	if err != nil {
		return "", Attachment{}, "", err
	}
	if res.IsError {
		return "", Attachment{}, strings.TrimSpace(res.Content), nil
	}
	content, truncated := capText(res.Content, limit)
	note := ""
	if truncated {
		note = fmt.Sprintf("\n[attached the first %d KB of it]", limit>>10)
	}
	sum := sha256.Sum256([]byte(content))
	where := redacted("mcp:" + server + ":" + uri)
	att = Attachment{Path: where, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(content)), Truncated: truncated || res.Truncated}
	return fenced("resource", fmt.Sprintf("server=%q uri=%q", server, redacted(uri)), content+note), att, "", nil
}
