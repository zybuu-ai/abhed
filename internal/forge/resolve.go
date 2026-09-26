package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/zybuu-ai/abhed/internal/hostgit"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Work is one resolution: the issue, the branch it is worked on, and where.
type Work struct {
	Issue  Issue
	Branch string
	Dir    string // the worktree
	Base   string
}

// Runner does the actual work in the worktree. It is the agent session; the
// resolver knows nothing about models.
type Runner func(ctx context.Context, dir, prompt string) error

// Prompt is what the agent is asked, built from the issue.
func Prompt(is Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resolve issue #%d: %s\n\n", is.Ref.Number, is.Title)
	if strings.TrimSpace(is.Body) != "" {
		b.WriteString(is.Body)
		b.WriteString("\n\n")
	}
	b.WriteString("Make the change in this checkout, add or update tests, and run the tests. " +
		"Do not commit and do not push: that is done for you once you are finished. " +
		"If the issue cannot be resolved, say why instead of guessing.")
	return b.String()
}

// Begin checks the repository out on a fresh branch in its own worktree, so
// the user's checkout is untouched while the agent works.
func Begin(ctx context.Context, repo string, is Issue, base string) (*Work, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git is not installed")
	}
	r := hostgit.New(ctx, repo)
	if _, err := git(ctx, r, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository", repo)
	}
	branch := fmt.Sprintf("abhed/issue-%d", is.Ref.Number)
	dir := filepath.Join(repo, ".abhed", "worktrees", fmt.Sprintf("issue-%d", is.Ref.Number))
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, err
	}
	start := "HEAD"
	if base != "" {
		if _, err := git(ctx, r, "rev-parse", "--verify", "origin/"+base); err == nil {
			start = "origin/" + base
		} else if _, err := git(ctx, r, "rev-parse", "--verify", base); err == nil {
			start = base
		}
	}
	// A branch left by an earlier attempt is replaced, not built on.
	_, _ = git(ctx, r, "worktree", "remove", "--force", dir)
	_, _ = git(ctx, r, "branch", "-D", branch)
	if _, err := git(ctx, r, "worktree", "add", "-b", branch, dir, start); err != nil {
		return nil, err
	}
	return &Work{Issue: is, Branch: branch, Dir: dir, Base: base}, nil
}

// ErrEmbeddedRepo refuses a commit when the agent left a repository inside
// the worktree: git would enter it, and run what its own configuration names.
var ErrEmbeddedRepo = errors.New("the change holds a git repository of its own")

// Commit records what the agent changed, as one commit that names the issue.
// The author is the repository's configured one: the person who ran this.
func (w *Work) Commit(ctx context.Context) (string, error) {
	if nested := embeddedRepo(w.Dir); nested != "" {
		return "", fmt.Errorf("%w (%s); remove it and resolve again", ErrEmbeddedRepo, nested)
	}
	r := hostgit.New(ctx, w.Dir)
	if _, err := git(ctx, r, "add", "-A"); err != nil {
		return "", err
	}
	status, err := git(ctx, r, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) == "" {
		return "", ErrNoChange
	}
	msg := fmt.Sprintf("Resolve #%d: %s\n\nSee %s", w.Issue.Ref.Number, w.Issue.Title, w.Issue.URL)
	if _, err := git(ctx, r, "commit", "-q", "-m", msg); err != nil {
		return "", err
	}
	return git(ctx, r, "rev-parse", "--short", "HEAD")
}

// embeddedRepo finds a .git, folder or file, below the worktree's top: a
// repository or submodule the agent made. It names the first, or "".
func embeddedRepo(dir string) string {
	found := ""
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil //nolint:nilerr // an unreadable entry holds no repository we can enter
		}
		if strings.EqualFold(d.Name(), ".git") && filepath.Dir(path) != dir {
			found, _ = filepath.Rel(dir, path)
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// PushURL is where the branch goes: the issue's own repository, built from
// the issue's address rather than read from a remote the agent could change.
func PushURL(ref Ref) (string, error) {
	if ref.Scheme != "https" {
		return "", fmt.Errorf("pushing needs an https repository address, not %s://", ref.Scheme)
	}
	return "https://" + ref.Host + "/" + ref.Owner + "/" + ref.Repo + ".git", nil
}

// Push sends the branch to the issue's repository over HTTPS. It pushes from
// a temporary bare repository that borrows the operator's objects and has no
// configuration of its own, so only the operator's global configuration
// shapes the connection: settings in a repository the agent could write (TLS
// checks, proxies, address rewrites, transports) do not apply. The forge's
// token is handed to git in its environment, scoped to the forge's host.
// caFile, when set, is the certificate authority of a self-hosted forge.
func (w *Work) Push(ctx context.Context, repo string, ref Ref, auth, caFile string) error {
	url, err := PushURL(ref)
	if err != nil {
		return err
	}
	return push(ctx, repo, w.Dir, url, "https://"+ref.Host+"/", w.Branch, auth, caFile, nil)
}

func push(ctx context.Context, repo, worktree, url, scope, branch, auth, caFile string, env []string) error {
	if err := globalConfigWritable(worktree); err != nil {
		return err
	}
	parent, err := pushParent()
	if err != nil {
		return err
	}
	src := hostgit.New(ctx, repo)
	sha, err := git(ctx, src, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return err
	}
	common, err := git(ctx, src, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	objects := filepath.Join(common, "objects")
	tmp, err := os.MkdirTemp(parent, "abhed-push-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	// Named with GIT_DIR on every command: -C finds a bare repository
	// implicitly, which safe.bareRepository=explicit refuses.
	bare := hostgit.New(ctx, tmp)
	at := append([]string{"GIT_DIR=" + tmp}, env...)
	run := func(args ...string) error {
		var errb bytes.Buffer
		cmd := bare.CommandWith(ctx, nil, at, args...)
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
		}
		return nil
	}
	if err := run("init", "-q", "--bare", tmp); err != nil {
		return err
	}
	alternates := []byte(objects + "\n")
	if err := os.WriteFile(filepath.Join(tmp, "objects", "info", "alternates"), alternates, 0o600); err != nil {
		return err
	}
	if err := run("update-ref", "refs/heads/"+branch, sha); err != nil {
		return err
	}
	config := [][2]string{{"http." + scope + ".extraHeader", "Authorization: " + auth}}
	if caFile != "" {
		config = append(config, [2]string{"http." + scope + ".sslCAInfo", caFile})
	}
	cmd := bare.CommandWith(ctx, config, at, "push", url, "refs/heads/"+branch+":refs/heads/"+branch)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "transport '") && strings.Contains(msg, "not allowed") {
			msg += fmt.Sprintf(". abhed resolve pushes over https only, and your git configuration "+
				"rewrites %s to another transport (a url.<base>.insteadOf); keep pushes on https with "+
				"`git config --global url.%s.pushInsteadOf %s`", scope, scope, scope)
		}
		return fmt.Errorf("git push: %s", msg)
	}
	return nil
}

// ownedByMe reports whether a file belongs to the current user. A variable so
// a test can stand in a foreign owner.
var ownedByMe = fileOwnedByMe

// writableAreas are the folders beyond the worktree that a sandboxed run can
// write. A variable so the tests, whose temp folders are among them, can
// name their own.
var writableAreas = sandbox.WritableAreas

// pushParent is ~/.abhed/push, where the temporary push repository is made:
// a folder sandboxed commands cannot write. Anything else refuses the push.
func pushParent() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("refusing to push: no home directory for ~/.abhed/push: %w", err)
	}
	dir := filepath.Join(home, ".abhed", "push")
	for _, a := range writableAreas() {
		if inside(dir, a) {
			return "", fmt.Errorf("refusing to push: %s is inside %s, which the run can write", dir, a)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || !ownedByMe(info) {
		return "", fmt.Errorf("refusing to push: %s must be a folder of your own (not a file or a link)", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- a folder needs x; 0700 is owner-only
		return "", err
	}
	return dir, nil
}

// globalConfigWritable refuses a push whose global git configuration the run
// could have written: a file, or the folder it would be made in, inside the
// worktree or a writable area. The rest of the checkout, such as a home
// directory holding dotfiles, is not the run's to write.
func globalConfigWritable(worktree string) error {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config"))
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		files = append(files, filepath.Join(x, "git", "config"))
	}
	areas := append([]string{worktree}, writableAreas()...)
	for _, f := range files {
		for _, a := range areas {
			if a != "" && (inside(f, a) || inside(filepath.Dir(f), a)) {
				return fmt.Errorf("refusing to push: your global git configuration %s is inside %s, "+
					"which the run can write", f, a)
			}
		}
	}
	return nil
}

// inside reports whether p, with links followed, is dir or under it; names
// compare without case where the disk usually ignores it.
func inside(p, dir string) bool {
	rp, rd := tools.RealPath(p), tools.RealPath(dir)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		rp, rd = strings.ToLower(rp), strings.ToLower(rd)
	}
	rel, err := filepath.Rel(rd, rp)
	return err == nil && filepath.IsLocal(rel)
}

// Diff summarises the change for the pull request body, from the trusted
// repository.
func (w *Work) Diff(ctx context.Context, repo string) string {
	out, err := git(ctx, hostgit.New(ctx, repo), "diff", "--stat", w.Branch+"~1", w.Branch)
	if err != nil {
		return ""
	}
	return out
}

// Cleanup removes the worktree; the branch stays, since it was pushed.
func (w *Work) Cleanup(ctx context.Context, repo string) {
	_, _ = git(ctx, hostgit.New(ctx, repo), "worktree", "remove", "--force", w.Dir)
}

// git runs one command on r, whose drivers were read once for the operation.
func git(ctx context.Context, r *hostgit.Repo, args ...string) (string, error) {
	cmd := r.Command(ctx, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}
