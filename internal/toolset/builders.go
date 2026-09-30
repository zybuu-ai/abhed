package toolset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/agent"
	"github.com/zybuu-ai/abhed/internal/agentdefs"
	"github.com/zybuu-ai/abhed/internal/index"
	"github.com/zybuu-ai/abhed/internal/k8s"
	"github.com/zybuu-ai/abhed/internal/managed"
	"github.com/zybuu-ai/abhed/internal/mcp"
	"github.com/zybuu-ai/abhed/internal/model"
	"github.com/zybuu-ai/abhed/internal/rag"
	"github.com/zybuu-ai/abhed/internal/remote"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/skills"
	"github.com/zybuu-ai/abhed/internal/tools"
	"github.com/zybuu-ai/abhed/internal/webfetch"
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

// WebFetchTool constructs web_fetch when enabled, or returns nil. It reads
// vault on each call, so a secret stored while the session runs is covered,
// and refuses a URL holding a stored value. With no vault it cannot check a
// URL, so it fetches nothing.
func WebFetchTool(cfg config.Config, vault *secrets.Store) *webfetch.Tool {
	if !cfg.WebFetch.Enabled {
		return nil
	}
	load := func() (*secrets.Redactor, error) {
		return nil, errors.New("no secrets store")
	}
	if vault != nil {
		load = vault.LoadRedactor
	}
	return &webfetch.Tool{AllowedHosts: cfg.WebFetch.AllowedHosts,
		Secrets: load, MaxChars: cfg.WebFetch.MaxChars}
}

// WebSearchTool constructs the web search tool when enabled. Returns nil, nil
// when the operator has left it off, which is the default.
func WebSearchTool(cfg config.Config) (*websearch.Tool, error) {
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

// AgentRoots are the operator's definition directories: agents.dirs, or
// ~/.abhed/agents. None when definitions are disabled.
func AgentRoots(cfg config.Config) []string {
	if cfg.Agents.Disabled {
		return nil
	}
	if dirs := cfg.Agents.Dirs; len(dirs) > 0 {
		return dirs
	}
	return []string{"~/.abhed/agents"}
}

// OfferedModels are the provider names a subagent may be given: configured,
// and offered to sessions.
func OfferedModels(cfg config.Config) []string {
	var out []string
	for name := range cfg.Model.Providers {
		if cfg.Offered(name) && !pinnedAway(cfg, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// pinnedAway reports a provider other than the one a managed model.default
// pins every model choice to.
func pinnedAway(cfg config.Config, name string) bool {
	return cfg.ManagedSets("model.default") && name != cfg.Model.Default
}

// ModelResolver is a subagent factory's model choice over the configuration:
// a name is looked up among the offered, configured providers and never
// treated as an endpoint. A managed model.default pins it, as it pins the
// session's own model. Its key comes from this process's environment, as
// the session's own does. An untrusted workspace file cannot add a provider,
// so it cannot add a name here.
func ModelResolver(cfg config.Config) func(string) (model.Adapter, error) {
	offered := OfferedModels(cfg)
	return func(name string) (model.Adapter, error) {
		avail := strings.Join(offered, ", ")
		if pinnedAway(cfg, name) {
			return nil, fmt.Errorf("the organisation's configuration pins the model to %s", cfg.Model.Default)
		}
		if !slices.Contains(offered, name) {
			return nil, fmt.Errorf("it is not a configured provider; available: %s", avail)
		}
		p, err := cfg.ProviderNamed(name)
		if err != nil {
			return nil, fmt.Errorf("%w; available: %s", err, avail)
		}
		a, err := p.Adapter()
		if err != nil {
			return nil, fmt.Errorf("%w; available: %s", err, avail)
		}
		return a, nil
	}
}

// LoadAgents loads the subagent definitions with the built-in roles: the
// managed ones, the workspace's when ws says they are trusted, and the
// operator's. One that does not load is reported, not fatal.
func LoadAgents(cfg config.Config, ws config.WorkspaceTrust, warn func(string, ...any)) *agent.Definitions {
	o := agentdefs.Options{
		ManagedDir: managed.AgentsDir,
		Dirs:       AgentRoots(cfg),
		Disabled:   cfg.Agents.Disabled,
		Models:     OfferedModels(cfg),
	}
	if ws.AgentsTrusted {
		o.Workspace = ws.AgentFiles()
	}
	defs, errs := agentdefs.Load(o)
	for _, err := range errs {
		warn("%v", err)
	}
	return agent.WithDefinitions(defs...)
}

// InfraTools constructs the cluster and remote-host tools. Both are off by
// default and both report why they are unavailable rather than silently
// registering nothing. A credential obtained at run time is read from vault by
// name and held by the session that obtained it, never by these tools; a nil
// vault offers no stored secrets.
func InfraTools(cfg config.Config, vault *secrets.Store, warn func(string, ...any)) []tools.Tool {
	var out []tools.Tool
	var secret func(string) (string, error)
	var secretNames []string
	if vault != nil {
		secret = vault.Value
		secretNames, _ = vault.Names() // unreadable lists nothing; a use reports why
	}

	if cfg.K8s.Enabled {
		mgr := k8s.NewManager(k8s.Config{
			Kubeconfig: cfg.K8s.Kubeconfig,
			Context:    cfg.K8s.Context,
			Namespace:  cfg.K8s.Namespace,
			// From the environment only: a token in a config file sits in a
			// directory the agent itself can read.
			Token:    os.Getenv("ABHED_K8S_TOKEN"),
			Clusters: LoginClusters(cfg),
			CAFile:   cfg.K8s.CAFile,
		})
		for _, c := range cfg.K8s.Clusters {
			if c.InsecureSkipTLSVerify {
				warn("k8s cluster %q skips TLS verification — "+
					"a token k8s_login sends to it can be read by anyone in the path", c.Name)
			}
		}
		out = append(out, k8s.GetTool{M: mgr},
			k8s.LoginTool{M: mgr, Secret: secret, SecretNames: secretNames})
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
		out = append(out, remote.Tool{R: reg}, remote.ConnectTool{R: reg, Secret: secret})
	}
	return out
}

// LoginClusters are the clusters k8s_login may send a stored token to.
func LoginClusters(cfg config.Config) []k8s.LoginCluster {
	out := make([]k8s.LoginCluster, 0, len(cfg.K8s.Clusters))
	for _, c := range cfg.K8s.Clusters {
		out = append(out, k8s.LoginCluster{Name: c.Name, Server: c.Server,
			CAFile: c.CAFile, InsecureSkipTLSVerify: c.InsecureSkipTLSVerify})
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
