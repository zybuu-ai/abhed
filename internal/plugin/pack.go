package plugin

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Pack fills m's file list from files (keyed by slash path, without the
// manifest or its signature) and, where m requests nothing of a kind, the
// capabilities the content uses. It returns the manifest's bytes, checked
// as a bundle would be. It is what a publisher runs before signing.
func Pack(m Manifest, files map[string][]byte) ([]byte, error) {
	m.Files = m.Files[:0]
	paths := make([]string, 0, len(files))
	for p := range files {
		if p == ManifestName || p == SignatureName {
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		m.Files = append(m.Files, File{Path: p, SHA256: Digest(files[p])})
	}
	probe := &Bundle{Manifest: &m, Files: files}
	need := probe.Needs()
	if len(m.Capabilities.Tools) == 0 {
		m.Capabilities.Tools = need.Tools
	}
	if len(m.Capabilities.Egress) == 0 {
		m.Capabilities.Egress = need.Egress
	}
	if len(m.Capabilities.Hooks) == 0 {
		m.Capabilities.Hooks = need.Hooks
	}
	if len(m.Capabilities.MCPServers) == 0 {
		m.Capabilities.MCPServers = need.MCPServers
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	all := map[string][]byte{ManifestName: data}
	for _, p := range paths {
		all[p] = files[p]
	}
	if _, err := FromFiles(all); err != nil {
		return nil, fmt.Errorf("the packed bundle does not check: %w", err)
	}
	return data, nil
}
