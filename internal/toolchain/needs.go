package toolchain

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// Need is a language the workspace looks like it needs, and why.
type Need struct {
	Lang string `json:"lang"`
	// Why is the marker file, or the kind of file, that says so.
	Why string `json:"why"`
}

// markers are files that name a project's language.
var markers = map[string]string{
	"go.mod": "go", "go.work": "go",
	"pyproject.toml": "python", "requirements.txt": "python", "setup.py": "python", "Pipfile": "python",
	"package.json": "node",
	"Cargo.toml":   "rust",
	"pom.xml":      "maven", "build.gradle": "gradle", "build.gradle.kts": "gradle", "settings.gradle": "gradle",
	"Gemfile":     "ruby",
	"global.json": "dotnet",
}

// extensions are source files that name a language, loose ones included.
var extensions = map[string]string{
	".go": "go", ".py": "python", ".js": "node", ".mjs": "node", ".cjs": "node", ".ts": "node",
	".rs": "rust", ".java": "java", ".kt": "java", ".rb": "ruby",
	".cs": "dotnet", ".csproj": "dotnet", ".fsproj": "dotnet", ".sln": "dotnet",
}

// skipped are folders a project's dependencies and builds live in.
var skipped = map[string]bool{".git": true, "node_modules": true, "vendor": true, "target": true, ".venv": true, "venv": true,
	"dist": true, "build": true, ".abhed": true, "__pycache__": true, ".gradle": true, "bin": true, "obj": true}

// Bounds of the look.
const (
	needsDepth   = 4
	needsEntries = 5000
)

// Needs are the languages the workspace looks like it needs: by its marker
// files and by its source files, loose ones too, looked for a few levels
// down. Maven and Gradle projects need Java too.
func Needs(workspace string) []Need {
	found := map[string]string{}
	add := func(lang, why string) {
		if _, ok := found[lang]; !ok {
			found[lang] = why
		}
	}
	n := 0
	root := filepath.Clean(workspace)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > needsEntries {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			if p != root && (skipped[d.Name()] || strings.Count(rel, string(filepath.Separator)) >= needsDepth) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if lang, ok := markers[d.Name()]; ok {
			add(lang, rel)
		} else if lang, ok := extensions[strings.ToLower(filepath.Ext(d.Name()))]; ok {
			add(lang, "*"+filepath.Ext(d.Name())+" files")
		}
		return nil
	})
	for _, build := range []string{"maven", "gradle"} {
		if why, ok := found[build]; ok {
			add("java", why)
		}
	}
	for lang, p := range PinsFrom(workspace) {
		add(lang, p.File)
	}
	var out []Need
	for _, l := range Langs {
		if why, ok := found[l.Name]; ok {
			out = append(out, Need{Lang: l.Name, Why: why})
		}
	}
	return out
}
