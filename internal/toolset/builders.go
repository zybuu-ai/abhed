package toolset

import (
	"context"
	"fmt"
	"os"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/rag"
	"github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/websearch"
)

// MCPConfigs is the configuration's MCP servers in the gateway's terms.
func MCPConfigs(cfg config.Config) []mcp.ServerConfig {
	out := make([]mcp.ServerConfig, 0, len(cfg.MCP.Servers))
	for _, s := range cfg.MCP.Servers {
		out = append(out, mcp.ServerConfig{
			Name: s.Name, Command: s.Command, Args: s.Args, Env: s.Env,
			URL: s.URL, Headers: s.Headers, HeadersEnv: s.HeadersEnv,
			Enabled: s.Enabled, AllowTools: s.AllowTools, Digest: s.Digest,
		})
	}
	return out
}

// WebSearchTool constructs the web search tool when enabled. Returns nil, nil
// when the operator has left it off, which is the default.
func WebSearchTool(cfg config.Config) (tools.Tool, error) {
	if !cfg.WebSearch.Enabled {
		return nil, nil
	}
	key := cfg.WebSearch.APIKey
	if key == "" && cfg.WebSearch.APIKeyEnv != "" {
		key = os.Getenv(cfg.WebSearch.APIKeyEnv)
	}
	p, err := websearch.New(websearch.Config{
		Provider:   cfg.WebSearch.Provider,
		APIKey:     key,
		BaseURL:    cfg.WebSearch.BaseURL,
		MaxResults: cfg.WebSearch.MaxResults,
	})
	if err != nil {
		return nil, err
	}
	return &websearch.Tool{Provider: p, Limit: cfg.WebSearch.MaxResults}, nil
}

// SkillRoots is where skills are looked FOR, as distinct from SkillDirs, which
// holds each loaded skill's own directory so its assets can be read.
//
// The two were easy to confuse and the confusion was silent: reloading from
// the skill directories scans inside individual skills and finds nothing, so a
// reload reported zero skills loaded while eleven were live.
func SkillRoots(cfg config.Config) []string {
	if cfg.Skills.Disabled {
		return nil
	}
	if dirs := cfg.Skills.Dirs; len(dirs) > 0 {
		return dirs
	}
	return []string{"~/.abhed/skills"}
}

// LoadSkills loads the configured skill directories and returns the registry
// plus its prompt listing. One malformed SKILL.md is reported, not fatal.
func LoadSkills(cfg config.Config, warn func(string, ...any)) (*skills.Registry, string) {
	if cfg.Skills.Disabled {
		return skills.NewRegistry(), ""
	}
	reg, errs := skills.Load(SkillRoots(cfg))
	for _, err := range errs {
		warn("%v", err)
	}
	return reg, reg.Listing()
}

// InfraTools constructs the cluster and remote-host tools. Both are off by
// default and both report why they are unavailable rather than silently
// registering nothing.
func InfraTools(cfg config.Config, warn func(string, ...any)) []tools.Tool {
	var out []tools.Tool

	if cfg.K8s.Enabled {
		mgr := k8s.NewManager(k8s.Config{
			Kubeconfig: cfg.K8s.Kubeconfig,
			Context:    cfg.K8s.Context,
			Namespace:  cfg.K8s.Namespace,
			// From the environment only: a token in a config file sits in a
			// directory the agent itself can read.
			Token: os.Getenv("ABHED_K8S_TOKEN"),
		})
		out = append(out, k8s.GetTool{M: mgr}, k8s.LoginTool{M: mgr})
		if cfg.K8s.AllowWrites {
			out = append(out, k8s.ApplyTool{M: mgr})
		}
	}

	// No len(Hosts) > 0 condition: ssh_connect is how a host gets declared in
	// the first place, so requiring one in config to reach the tool that adds
	// them was the bug — a user with a VM and a key had no way in.
	if cfg.SSH.Enabled {
		hosts := make([]remote.HostConfig, 0, len(cfg.SSH.Hosts))
		for _, h := range cfg.SSH.Hosts {
			hosts = append(hosts, remote.HostConfig{
				Name: h.Name, Addr: h.Addr, User: h.User,
				IdentityFile: h.IdentityFile, PasswordEnv: h.PasswordEnv,
				KnownHostsFile:           h.KnownHostsFile,
				InsecureSkipHostKeyCheck: h.InsecureSkipHostKeyCheck,
			})
			if h.InsecureSkipHostKeyCheck {
				warn("ssh host %q skips host key verification — it cannot detect a machine-in-the-middle", h.Name)
			}
		}
		reg, errs := remote.NewRegistry(hosts)
		for _, err := range errs {
			warn("ssh: %v", err)
		}
		out = append(out, remote.Tool{R: reg}, remote.ConnectTool{R: reg})
	}
	return out
}

// RAGTools constructs a tool per enabled corpus. A corpus that cannot be
// configured is reported and skipped rather than failing startup: one broken
// endpoint should not take the whole agent down.
func RAGTools(cfg config.Config, warn func(string, ...any)) []tools.Tool {
	var out []tools.Tool
	for _, c := range cfg.RAG.Corpora {
		if !c.Enabled {
			continue
		}
		headers := map[string]string{}
		for k, v := range c.Headers {
			headers[k] = v
		}
		for k, envVar := range c.HeadersEnv {
			if v := os.Getenv(envVar); v != "" {
				headers[k] = v
			} else {
				warn("rag corpus %q needs %s in the environment; skipping", c.Name, envVar)
				headers = nil
				break
			}
		}
		if headers == nil {
			continue
		}
		r, err := rag.New(rag.Config{
			Name: c.Name, Description: c.Description, URL: c.URL, Method: c.Method,
			Headers: headers, QueryField: c.QueryField, QueryParam: c.QueryParam,
			TopKField: c.TopKField, TopK: c.TopK, Body: c.Body,
			ResultsPath: c.ResultsPath, TextField: c.TextField,
			SourceField: c.SourceField, TitleField: c.TitleField, ScoreField: c.ScoreField,
		})
		if err != nil {
			warn("rag corpus %q: %v", c.Name, err)
			continue
		}
		out = append(out, &rag.Tool{R: r})
	}
	return out
}

// IndexOptions mirrors what OpenIndex uses, so a reindex triggered from the
// settings surface rebuilds on the same terms as the startup build rather than
// quietly dropping the vector tier.
func IndexOptions(cfg config.Config) index.BuildOptions {
	opts := index.DefaultBuildOptions()
	opts.Embed = cfg.Retrieval.Embed && cfg.Retrieval.EmbedBaseURL != ""
	return opts
}

// OpenIndex builds the retrieval index for a workspace.
func OpenIndex(ctx context.Context, cfg config.Config, workspace string) (*index.Index, error) {
	if workspace == "" {
		return nil, fmt.Errorf("no workspace to index")
	}
	ix := index.New(workspace)
	opts := index.DefaultBuildOptions()

	if cfg.Retrieval.Embed && cfg.Retrieval.EmbedBaseURL != "" {
		provider, _ := cfg.Provider()
		ix = ix.WithEmbedder(index.NewOpenAIEmbedder(
			cfg.Retrieval.EmbedBaseURL, provider.APIKey,
			cfg.Retrieval.EmbedModel, cfg.Retrieval.EmbedDims))
		opts.Embed = true
	}
	if err := ix.Build(ctx, opts); err != nil {
		return nil, err
	}
	return ix, nil
}
