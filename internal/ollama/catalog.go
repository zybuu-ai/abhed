package ollama

import "strings"

// Curated is one model Abhed suggests for Ollama. Every tag was checked
// against the Ollama library (ollama.com/library/<name>/tags) and the
// registry manifest (registry.ollama.ai/v2/library/<name>/manifests/<tag>)
// on 24 October 2026; Bytes is the sum of that manifest's layers then. A
// pull asks the registry again for the size it shows, so a tag that is
// republished larger is shown as it is.
type Curated struct {
	Tag   string `json:"tag"`
	Label string `json:"label"`
	// Bytes is the download size when the tag was checked.
	Bytes int64 `json:"bytes"`
	// MinRAMGB is the system memory below which the model is warned
	// against: its weights, the context Abhed uses, and room for the system.
	MinRAMGB int `json:"min_ram_gb"`
}

// Catalog is the curated list, smallest first. Qwen3-Coder is listed by its
// explicit 30b tag, which is what qwen3-coder:latest pointed to (the same
// manifest digest) when checked.
var Catalog = []Curated{
	{Tag: "qwen2.5-coder:7b", Label: "Qwen2.5-Coder 7B", Bytes: 4683087561, MinRAMGB: 8},
	{Tag: "qwen3:8b", Label: "Qwen3 8B", Bytes: 5225388164, MinRAMGB: 8},
	{Tag: "qwen3-coder:30b", Label: "Qwen3-Coder 30B", Bytes: 18556700761, MinRAMGB: 24},
	{Tag: "gemma4:26b", Label: "Gemma 4 26B", Bytes: 18731025629, MinRAMGB: 24},
	{Tag: "gemma4:31b", Label: "Gemma 4 31B", Bytes: 20385458234, MinRAMGB: 32},
}

// Lookup finds a curated model by tag; a tag without one means :latest.
func Lookup(tag string) (Curated, bool) {
	tag = normalTag(tag)
	for _, c := range Catalog {
		if c.Tag == tag {
			return c, true
		}
	}
	return Curated{}, false
}

// normalTag adds :latest to a tag that names none.
func normalTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if i := strings.LastIndex(tag, "/"); !strings.Contains(tag[i+1:], ":") {
		return tag + ":latest"
	}
	return tag
}

// RAMNeededGB is the system memory a model wants: the curated figure, or
// for any other model its download size plus a quarter and 2 GB for the
// system, rounded up. The second is an estimate and is said to be one.
func RAMNeededGB(tag string, bytes int64) (gb int, estimate bool) {
	if c, ok := Lookup(tag); ok {
		return c.MinRAMGB, false
	}
	if bytes <= 0 {
		return 0, true
	}
	const g = 1 << 30
	need := bytes + bytes/4 + 2*g
	return int((need + g - 1) / g), true
}

// Smaller is the largest curated model that fits in memGB of memory and is
// smaller than tag, or false when there is none.
func Smaller(tag string, memGB int) (Curated, bool) {
	var best Curated
	found := false
	cur, known := Lookup(tag)
	for _, c := range Catalog {
		if c.MinRAMGB > memGB || (known && c.Bytes >= cur.Bytes) || c.Tag == normalTag(tag) {
			continue
		}
		if !found || c.Bytes > best.Bytes {
			best, found = c, true
		}
	}
	return best, found
}
