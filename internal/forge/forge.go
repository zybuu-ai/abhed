// Package forge turns an issue on a git host into a branch and a pull
// request. GitHub, GitLab and Gitea (Forgejo) are one interface, and a
// self-hosted instance of each, with its own certificate authority, is the
// ordinary case rather than the exception: the deployments Abhed is built
// for run their own.
//
// The token comes from the environment or the secrets store. It is never
// written to the event record and never enters the sandbox: fetching the
// issue and opening the request happen outside the session, in this package.
package forge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Ref names one issue on one host.
type Ref struct {
	Scheme string
	Host   string
	Owner  string
	Repo   string
	Number int
}

func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d on %s", r.Owner, r.Repo, r.Number, r.Host) }

// Issue is what the agent is given to work from.
type Issue struct {
	Ref   Ref
	Title string
	Body  string
	URL   string
}

// PullRequest is what is opened when the work is done.
type PullRequest struct {
	Head  string // the branch with the change
	Base  string // the branch it targets
	Title string
	Body  string
}

// Forge is one git host.
type Forge interface {
	Kind() string
	Issue(ctx context.Context, ref Ref) (Issue, error)
	OpenPullRequest(ctx context.Context, ref Ref, pr PullRequest) (string, error)
	// PushAuth is the Authorization header value git uses to push over HTTPS.
	PushAuth() string
	// DefaultBranch reports the repository's default branch, for the base.
	DefaultBranch(ctx context.Context, ref Ref) (string, error)
}

// Kinds this package speaks.
const (
	KindGitHub = "github"
	KindGitLab = "gitlab"
	KindGitea  = "gitea"
)

var issuePath = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?/(?:-/)?(?:issues|pull|pulls|merge_requests)/(\d+)/?$`)

// Parse reads an issue URL: https://host/owner/repo/issues/N, GitLab's
// /-/issues/N, and nested GitLab groups as owner "group/sub".
func Parse(raw string) (Ref, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return Ref{}, fmt.Errorf("%q is not an issue URL; expected https://host/owner/repo/issues/N", raw)
	}
	path := u.Path
	// GitLab subgroups: everything before /-/issues is the project path.
	if i := strings.Index(path, "/-/issues/"); i > 0 {
		n, err := strconv.Atoi(strings.Trim(path[i+len("/-/issues/"):], "/"))
		if err != nil {
			return Ref{}, fmt.Errorf("%q: no issue number", raw)
		}
		project := strings.Trim(path[:i], "/")
		slash := strings.LastIndex(project, "/")
		if slash < 0 {
			return Ref{}, fmt.Errorf("%q: no project path", raw)
		}
		return Ref{Scheme: u.Scheme, Host: u.Host, Owner: project[:slash], Repo: project[slash+1:], Number: n}, nil
	}
	m := issuePath.FindStringSubmatch(path)
	if m == nil {
		return Ref{}, fmt.Errorf("%q is not an issue URL; expected https://host/owner/repo/issues/N", raw)
	}
	n, _ := strconv.Atoi(m[3])
	return Ref{Scheme: u.Scheme, Host: u.Host, Owner: m[1], Repo: m[2], Number: n}, nil
}

// Options configures a connection to one host.
type Options struct {
	// Kind is github, gitlab or gitea. Empty is inferred: github.com and
	// gitlab.com by name, anything else by asking the host.
	Kind string
	// Token is the API token. Empty falls back to GITHUB_TOKEN, GITLAB_TOKEN
	// or GITEA_TOKEN, by kind.
	Token string
	// CAFile adds a certificate authority for a self-hosted instance.
	CAFile string
	// APIBase overrides the API root, e.g. https://ghe.example/api/v3.
	APIBase string
	// Client, when set, replaces the HTTP client (tests).
	Client *http.Client
}

// New connects to the host that ref names.
func New(ctx context.Context, ref Ref, opts Options) (Forge, error) {
	client := opts.Client
	if client == nil {
		var err error
		client, err = httpClient(opts.CAFile)
		if err != nil {
			return nil, err
		}
	}
	kind := opts.Kind
	if kind == "" {
		kind = inferKind(ctx, client, ref)
	}
	token := opts.Token
	if token == "" {
		token = os.Getenv(map[string]string{KindGitHub: "GITHUB_TOKEN", KindGitLab: "GITLAB_TOKEN", KindGitea: "GITEA_TOKEN"}[kind])
	}
	if token == "" {
		return nil, fmt.Errorf("no token for %s: set %s_TOKEN, or store it with `abhed secret set %s_TOKEN`",
			ref.Host, strings.ToUpper(kind), strings.ToUpper(kind))
	}
	base := opts.APIBase
	switch kind {
	case KindGitHub:
		if base == "" {
			base = ref.Scheme + "://api.github.com"
			if ref.Host != "github.com" {
				base = ref.Scheme + "://" + ref.Host + "/api/v3"
			}
		}
		return &github{rest{client, base, token}}, nil
	case KindGitLab:
		if base == "" {
			base = ref.Scheme + "://" + ref.Host + "/api/v4"
		}
		return &gitlab{rest{client, base, token}}, nil
	case KindGitea:
		if base == "" {
			base = ref.Scheme + "://" + ref.Host + "/api/v1"
		}
		return &gitea{rest{client, base, token}}, nil
	}
	return nil, fmt.Errorf("unknown forge kind %q: want github, gitlab or gitea", kind)
}

func httpClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}
	pem, err := os.ReadFile(caFile) // #nosec G304 -- the operator names the CA file
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificate", caFile)
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}

// inferKind asks an unfamiliar host which API it serves. GitLab answers on
// /api/v4/version with 401, Gitea on /api/v1/version with 200; anything else
// is taken for a GitHub Enterprise host.
func inferKind(ctx context.Context, client *http.Client, ref Ref) string {
	switch ref.Host {
	case "github.com":
		return KindGitHub
	case "gitlab.com":
		return KindGitLab
	}
	probe := func(path string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.Scheme+"://"+ref.Host+path, nil)
		if err != nil {
			return 0
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := probe("/api/v1/version"); code == http.StatusOK {
		return KindGitea
	}
	if code := probe("/api/v4/version"); code == http.StatusUnauthorized || code == http.StatusOK {
		return KindGitLab
	}
	return KindGitHub
}

// rest is the shared HTTP plumbing.
type rest struct {
	client *http.Client
	base   string
	token  string
}

func (r rest) do(ctx context.Context, method, path string, auth string, in any, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func basic(user, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
}

// ---- GitHub and GitHub Enterprise

type github struct{ rest }

func (g *github) Kind() string     { return KindGitHub }
func (g *github) PushAuth() string { return basic("x-access-token", g.token) }

func (g *github) Issue(ctx context.Context, ref Ref) (Issue, error) {
	var out struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/%d", ref.Owner, ref.Repo, ref.Number), "Bearer "+g.token, nil, &out); err != nil {
		return Issue{}, err
	}
	return Issue{Ref: ref, Title: out.Title, Body: out.Body, URL: out.HTMLURL}, nil
}

func (g *github) DefaultBranch(ctx context.Context, ref Ref) (string, error) {
	var out struct {
		Default string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s", ref.Owner, ref.Repo), "Bearer "+g.token, nil, &out); err != nil {
		return "", err
	}
	return out.Default, nil
}

func (g *github) OpenPullRequest(ctx context.Context, ref Ref, pr PullRequest) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	in := map[string]any{"title": pr.Title, "head": pr.Head, "base": pr.Base, "body": pr.Body}
	if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", ref.Owner, ref.Repo), "Bearer "+g.token, in, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// ---- GitLab

type gitlab struct{ rest }

func (g *gitlab) Kind() string     { return KindGitLab }
func (g *gitlab) PushAuth() string { return basic("oauth2", g.token) }

func (g *gitlab) project(ref Ref) string { return url.PathEscape(ref.Owner + "/" + ref.Repo) }

func (g *gitlab) Issue(ctx context.Context, ref Ref) (Issue, error) {
	var out struct {
		Title  string `json:"title"`
		Body   string `json:"description"`
		WebURL string `json:"web_url"`
	}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/issues/%d", g.project(ref), ref.Number), "Bearer "+g.token, nil, &out); err != nil {
		return Issue{}, err
	}
	return Issue{Ref: ref, Title: out.Title, Body: out.Body, URL: out.WebURL}, nil
}

func (g *gitlab) DefaultBranch(ctx context.Context, ref Ref) (string, error) {
	var out struct {
		Default string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, "/projects/"+g.project(ref), "Bearer "+g.token, nil, &out); err != nil {
		return "", err
	}
	return out.Default, nil
}

func (g *gitlab) OpenPullRequest(ctx context.Context, ref Ref, pr PullRequest) (string, error) {
	var out struct {
		WebURL string `json:"web_url"`
	}
	in := map[string]any{"title": pr.Title, "source_branch": pr.Head, "target_branch": pr.Base, "description": pr.Body, "remove_source_branch": true}
	if err := g.do(ctx, http.MethodPost, "/projects/"+g.project(ref)+"/merge_requests", "Bearer "+g.token, in, &out); err != nil {
		return "", err
	}
	return out.WebURL, nil
}

// ---- Gitea and Forgejo

type gitea struct{ rest }

func (g *gitea) Kind() string     { return KindGitea }
func (g *gitea) PushAuth() string { return basic("abhed", g.token) }

func (g *gitea) Issue(ctx context.Context, ref Ref) (Issue, error) {
	var out struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/%d", ref.Owner, ref.Repo, ref.Number), "token "+g.token, nil, &out); err != nil {
		return Issue{}, err
	}
	return Issue{Ref: ref, Title: out.Title, Body: out.Body, URL: out.HTMLURL}, nil
}

func (g *gitea) DefaultBranch(ctx context.Context, ref Ref) (string, error) {
	var out struct {
		Default string `json:"default_branch"`
	}
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s", ref.Owner, ref.Repo), "token "+g.token, nil, &out); err != nil {
		return "", err
	}
	return out.Default, nil
}

func (g *gitea) OpenPullRequest(ctx context.Context, ref Ref, pr PullRequest) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	in := map[string]any{"title": pr.Title, "head": pr.Head, "base": pr.Base, "body": pr.Body}
	if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", ref.Owner, ref.Repo), "token "+g.token, in, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// ErrNoChange is returned by a resolver whose agent changed nothing.
var ErrNoChange = errors.New("the agent made no change to commit")
