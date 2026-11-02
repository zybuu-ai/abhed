package app

import (
	"fmt"
	"strings"
	"sync"

	"github.com/zybuu-ai/abhed/internal/embedded"
	"github.com/zybuu-ai/abhed/internal/termline"
	"github.com/zybuu-ai/abhed/internal/toolchain"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A command typed in Studio's terminal that the sandbox refused a package
// registry (npm install, go get through the egress proxy) is told which
// language's registry it was and how to open it, in the terminal itself,
// and Studio is sent _abhed/toolchain/registry_refused to offer the person
// its one-click opening, as the agent's path offers its ask.

// registryWatch is one terminal's watch: the tail of its output, for a
// refusal split across chunks, and the languages already offered.
type registryWatch struct {
	mu      sync.Mutex
	tail    string
	offered map[string]bool
}

// watchRegistries looks at a chunk of t's output for a refused registry.
func (c *acpConn) watchRegistries(t *acpTerminal, chunk []byte) {
	w := &t.reg
	w.mu.Lock()
	text := w.tail + termline.PlainText(chunk)
	if len(text) > 4096 {
		text = text[len(text)-4096:]
	}
	w.tail = text
	if !tools.ProxyRefused(text) {
		w.mu.Unlock()
		return
	}
	lang, ok := toolchain.RegistryOf("", text)
	if !ok || w.offered[lang] {
		w.mu.Unlock()
		return
	}
	if w.offered == nil {
		w.offered = map[string]bool{}
	}
	w.offered[lang] = true
	w.tail = ""
	w.mu.Unlock()

	l, _ := toolchain.Find(lang)
	hosts := toolchain.Registries[lang]
	canOpen, why := false, "toolchains are not available in this session"
	if parts, e := innerOf(t.session); e == nil && parts.Set != nil && parts.Set.Toolchains != nil {
		tc := parts.Set.Toolchains
		if tc.Off {
			why = toolchain.ErrOff.Error()
		} else if tc.Registries == nil || tc.Open == nil {
			why = "this session opens no package registries"
		} else if _, reason := tc.Registries(lang); reason != "" {
			why = reason
		} else {
			canOpen, why = true, ""
		}
	}
	note := fmt.Sprintf("%s's package registry (%s) is closed to this sandbox's commands.", l.Display, strings.Join(hosts, ", "))
	if canOpen {
		note += fmt.Sprintf(" Open it from Toolchains, Open %s's package registries, or ask the agent; then run the command again.", l.Display)
	} else {
		note += " It cannot be opened here: " + why + "."
	}
	c.show(t, []byte("\r\nabhed: "+note+"\r\n"))
	c.notification("_abhed/toolchain/registry_refused", map[string]any{"sessionId": t.session.id, "terminalId": t.id,
		"language": lang, "hosts": hosts, "canOpen": canOpen, "why": why, "message": note})
}

// terminalNetwork is what a terminal can reach, for its header: the
// package registries open to the session's commands now, by agreement or by
// configuration, and a line that says so, the network the agent's commands have.
func terminalNetwork(parts embedded.Parts, network bool) ([]string, string) {
	if network {
		return []string{}, "network: on, as the session's commands have it"
	}
	open := []string{}
	if parts.Set != nil && parts.Set.Toolchains != nil && parts.Set.Toolchains.Reachable != nil {
		for _, h := range toolchain.AllRegistries() {
			if parts.Set.Toolchains.Reachable(h) {
				open = append(open, h)
			}
		}
	}
	if len(open) > 0 {
		return open, "network: off but the package registries open to this session (" + strings.Join(open, ", ") + "), as the agent's commands have it"
	}
	return open, "network: off, as the agent's commands have it; a language's package registries open to both once you agree (Toolchains, or when the agent asks)"
}
