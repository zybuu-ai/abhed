// Package sandboxconfig builds the sandbox a configuration asks for, so the
// command line and the SDK cannot read sandbox settings differently.
package sandboxconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/egress"
	"github.com/zybuu-ai/abhed/internal/nlink"
	"github.com/zybuu-ai/abhed/internal/sandbox"
	"github.com/zybuu-ai/abhed/internal/secrets"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// Build selects an execution backend meeting the configured minimum tier; Select
// never downgrades. stateRoots' .abhed is state too, as a worktree's repository's is.
func Build(cfg config.Config, workspace string, stateRoots ...string) (sandbox.Sandbox, error) {
	p, err := Policy(cfg, workspace, stateRoots...)
	if err != nil {
		return nil, err
	}
	return sandbox.Select(p)
}

// Policy is the sandbox policy a configuration asks for, after checking its
// state paths. Build selects a backend for it.
func Policy(cfg config.Config, workspace string, stateRoots ...string) (sandbox.Policy, error) {
	return policy(cfg, workspace, workspace, stateRoots)
}

// InstancePolicy is Policy for a control-plane sandbox working in its own
// folder, workspace, beside serve's serveWorkspace: state and secret paths
// are resolved against serve's workspace, as serve reads them, and its
// .abhed is denied too.
func InstancePolicy(cfg config.Config, workspace, serveWorkspace string) (sandbox.Policy, error) {
	if serveWorkspace == "" {
		serveWorkspace = workspace
	}
	return policy(cfg, workspace, serveWorkspace, []string{serveWorkspace})
}

// policy is Policy with the state paths resolved against base.
func policy(cfg config.Config, workspace, base string, stateRoots []string) (sandbox.Policy, error) {
	if err := CheckStatePaths(cfg, base, stateRoots...); err != nil {
		return sandbox.Policy{}, err
	}
	if base != workspace {
		// The links in the sandbox's own folder are checked as well.
		if err := checkLinks(cfg, base, []string{workspace}); err != nil {
			return sandbox.Policy{}, err
		}
	}
	p := sandbox.DefaultPolicy(workspace)
	if cfg.Sandbox.MinTier != "" {
		p.MinTier = sandbox.Tier(cfg.Sandbox.MinTier)
	}
	p.AllowNetwork = cfg.Sandbox.AllowNetwork
	if cfg.Sandbox.Network == config.NetworkAllowlist {
		pol, err := egress.Compile(cfg.Egress)
		if err != nil {
			return sandbox.Policy{}, err
		}
		// The allowlist replaces allow_network: the proxy is the only way out.
		p.Egress, p.AllowNetwork = pol, false
	}
	p.ReadOnlyPaths = cfg.Sandbox.ReadOnlyPaths
	p.WriteProtected = cfg.Sandbox.WriteProtected
	p.ProtectGit, p.GitRequired = cfg.Sandbox.GitProtected()
	p.ScopeSignals = cfg.Sandbox.ScopeSignals
	p.StatePaths = StatePaths(cfg, base)
	for _, r := range stateRoots {
		if filepath.Clean(r) != filepath.Clean(workspace) {
			p.StatePaths = append(p.StatePaths, filepath.Join(r, tools.StateDir))
		}
	}
	// The command line's sign-in tokens and client keys, wherever kept.
	p.StatePaths = append(p.StatePaths, config.ProfileFiles()...)
	// A control-plane sandbox's own folder may lie in the state folder: the
	// rest of it is hidden around that folder, as its siblings are.
	for _, f := range ControlPlaneFolders(cfg) {
		if holds(f, workspace) {
			p.Hidden = append(p.Hidden, f)
		} else {
			p.StatePaths = append(p.StatePaths, f)
		}
	}
	if cfg.Sandbox.MaxMemoryMB > 0 {
		p.MaxMemoryMB = cfg.Sandbox.MaxMemoryMB
	}
	if cfg.Sandbox.MaxProcs > 0 {
		p.MaxProcs = cfg.Sandbox.MaxProcs
	}
	p.MaxCPUPercent = cfg.Sandbox.MaxCPUPercent
	p.SharedCaches = cfg.Sandbox.SharedCaches
	p.Tier = sandbox.Tier(cfg.Sandbox.Tier)
	p.CPUPercent = cfg.Fence.CPUPercent
	p.FenceSocketcall = cfg.Fence.Socketcall
	p.SkillDirs = skillDirs(cfg)
	return p, nil
}

// skillDirs are the configured skills.dirs, made absolute as skills are
// loaded from them: ~/ is the home folder, and a relative one is taken from
// the folder Abhed runs in.
func skillDirs(cfg config.Config) []string {
	if cfg.Skills.Disabled {
		return nil
	}
	home, _ := os.UserHomeDir()
	var out []string
	for _, d := range cfg.Skills.Dirs {
		if rest, ok := strings.CutPrefix(d, "~/"); ok && home != "" {
			d = filepath.Join(home, rest)
		}
		if abs, err := filepath.Abs(d); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// ControlPlaneDB is the SQLite file serve's control plane keeps sandboxes
// in, or "" when it keeps none: the control plane is off or uses Postgres.
func ControlPlaneDB(cfg config.Config) string {
	if !cfg.ControlPlane.Enabled || cfg.Storage.Driver == "postgres" {
		return ""
	}
	if p := cfg.ControlPlane.SQLitePath; p != "" {
		return filepath.Clean(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, tools.StateDir, "controlplane", "sandboxes.db")
}

// controlPlaneFiles are the database and the files SQLite keeps beside it.
func controlPlaneFiles(cfg config.Config) []string {
	db := ControlPlaneDB(cfg)
	if db == "" {
		return nil
	}
	return []string{db, db + "-wal", db + "-shm", db + "-journal"}
}

// ControlPlaneFolders are the control plane's state folder, which holds the
// relay's host key, snapshots and the sandboxes' own folders, and a
// snapshot_dir configured elsewhere. No command may read or write them,
// apart from its own sandbox's folder inside one.
func ControlPlaneFolders(cfg config.Config) []string {
	var out []string
	if d, err := config.ControlPlaneStateDir(); err == nil {
		out = append(out, d)
	}
	if cfg.ControlPlane.SnapshotDir != "" {
		if d, err := cfg.SnapshotDir(); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// ToolStatePaths are StatePaths with the command line's profiles and client
// keys, and the control plane's secrets in its state folder, the relay's
// host key and the snapshots, for the file tools
// and the server's readers to refuse. The folder itself is not one: a
// per-session sandbox's workspace lies in it.
func ToolStatePaths(cfg config.Config, workspace string) []string {
	out := append(StatePaths(cfg, workspace), config.ProfileFiles()...)
	if p, err := cfg.SSHHostKeyFile(); err == nil {
		out = append(out, p)
	}
	if p, err := cfg.SnapshotDir(); err == nil {
		out = append(out, p)
	}
	return out
}

// holds reports whether dir is or holds path, either as written or with
// links followed.
func holds(dir, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	return under([]string{abs, tools.RealPath(abs)}, []string{dir})
}

// StatePaths are the files holding Abhed's state that a configuration can put
// outside .abhed: the local accounts, the secrets store, the per-account
// stores, a managed record directory and the control plane's database. The
// tools, the server and the sandbox all refuse them.
func StatePaths(cfg config.Config, workspace string) []string {
	users := cfg.Auth.UsersFile
	if users == "" {
		users = filepath.Join(workspace, ".abhed", "users.json")
	} else if !filepath.IsAbs(users) {
		users = filepath.Join(workspace, users)
	}
	out := []string{users}
	// TLS keys and the command-line token key: whoever reads them can pose as
	// the server or sign tokens for any account.
	out = append(out, cfg.ServerSecretFiles(workspace)...)
	if path, err := secrets.DefaultPath(); err == nil {
		out = append(out, path)
	}
	if dir, err := secrets.AccountsDir(); err == nil {
		out = append(out, dir)
	}
	// The local record, when the managed configuration moves it out of
	// ~/.abhed, or when ~/.abhed/records is a link to somewhere else: the
	// agent can neither read nor write it where it really is.
	records := cfg.Record.Dir
	if records == "" {
		if home, err := os.UserHomeDir(); err == nil {
			records = filepath.Join(home, ".abhed", "records")
		}
	}
	if records != "" {
		if cfg.Record.Dir != "" {
			out = append(out, records)
		}
		if real, err := filepath.EvalSymlinks(records); err == nil && real != records {
			out = append(out, real)
		}
	}
	out = append(out, controlPlaneFiles(cfg)...)
	return append(out, cfg.Record.Also...)
}

// CheckStatePaths refuses a configured state file that commands could reach:
// one inside the workspace, an added directory, a temp folder or a toolchain
// cache, unless it is under the workspace's or the home directory's .abhed,
// which the sandbox denies whole. A deny rule on the file alone does not hold
// there, since a command can rename a folder above it or plant a link where
// it will be created. A path that does not exist yet is judged by its deepest
// existing parent, with links followed.
func CheckStatePaths(cfg config.Config, workspace string, stateRoots ...string) error {
	// A .abhed is a shield only where it really is: not a link to a folder
	// elsewhere, which the sandbox's rule for it would not cover.
	var shielded, homeShield []string
	bases := []string{workspace}
	if home, err := os.UserHomeDir(); err == nil {
		bases = append(bases, home)
	}
	for i, b := range bases {
		abs, err := filepath.Abs(b)
		if err != nil {
			continue
		}
		want := filepath.Join(tools.RealPath(abs), ".abhed")
		if tools.RealPath(filepath.Join(abs, ".abhed")) == want {
			shielded = append(shielded, want)
			if i == 1 {
				homeShield = []string{want}
			}
		}
	}
	areas := append([]string{workspace}, cfg.AdditionalDirs...)
	areas = append(areas, sandbox.WritableAreas()...)
	if cfg.ControlPlane.Enabled && cfg.ControlPlane.WorkspaceRoot != "" {
		// Every sandbox's own folder is somewhere its commands write.
		areas = append(areas, cfg.ControlPlane.WorkspaceRoot)
	}
	db := controlPlaneFiles(cfg)
	if len(db) > 0 && !filepath.IsAbs(db[0]) {
		return fmt.Errorf("refusing to start: control_plane.sqlite_path %q is not an absolute path", db[0])
	}
	for _, p := range StatePaths(cfg, workspace) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		real := tools.RealPath(abs)
		// The control plane's database stays out of every workspace, its
		// .abhed included; only the home directory's shields it.
		shields := shielded
		if slices.Contains(db, p) {
			shields = homeShield
		}
		if inside(real, shields) {
			continue
		}
		for _, a := range areas {
			if under([]string{abs, real}, []string{a}) {
				what, where := "auth.users_file, ABHED_SECRETS_FILE and control_plane.sqlite_path", " (the home directory's only, for the database)"
				if s := stateSetting(cfg, workspace, p); s != "" {
					what, where = s, ""
					if slices.Contains(db, p) {
						where = " (the home directory's only)"
					}
				}
				return fmt.Errorf("refusing to start: the state file %s is inside %s, where commands "+
					"can write and could move it or replace it. Keep %s outside the workspace, added "+
					"directories, sandbox folders, temp folders and caches, or under the workspace's "+
					"or the home directory's .abhed%s", p, a, what, where)
			}
		}
	}
	return checkLinks(cfg, workspace, stateRoots)
}

// stateSetting names what puts the state file p where it is: a setting, or
// the variable for the secrets store; "" when nothing names it alone.
func stateSetting(cfg config.Config, workspace, p string) string {
	named := map[string]string{}
	add := func(path, setting string) {
		if path != "" {
			if _, ok := named[path]; !ok {
				named[path] = setting
			}
		}
	}
	if cfg.Auth.UsersFile != "" {
		add(StatePaths(cfg, workspace)[0], "auth.users_file")
	}
	if path, err := secrets.DefaultPath(); err == nil {
		add(path, "ABHED_SECRETS_FILE")
	}
	for _, f := range controlPlaneFiles(cfg) {
		add(f, "control_plane.sqlite_path")
	}
	if cfg.Server.TLS.Enabled() {
		t := cfg.ResolvedTLS(workspace)
		add(t.KeyFile, "server.tls.key_file")
		for i, c := range t.AdditionalCertificates {
			add(c.KeyFile, fmt.Sprintf("server.tls.additional_certificates[%d].key_file", i))
		}
		add(t.ClientCAFile, "server.tls.client_ca_file")
	}
	if ha := cfg.ResolvedHA(workspace); ha.Enabled {
		add(ha.RelayKeyFile, "control_plane.ha.relay_key_file")
		add(ha.RelayCAFile, "control_plane.ha.relay_ca_file")
	}
	if cfg.Auth.CLI.Enabled {
		if k, err := cfg.CLIKeyFile(workspace); err == nil {
			for _, f := range []string{k, k + ".revoked", k + config.CLIEditionStateSuffix} {
				add(f, "auth.cli.key_file")
			}
		}
	}
	if w := cfg.ResolvedWorkload(workspace); w.Enabled {
		if k, err := cfg.WorkloadKeyFile(workspace); err == nil {
			add(k, "auth.workload.key_file")
			add(k+config.CLIEditionStateSuffix, "auth.workload.key_file")
		}
		add(w.JWTBundleFile, "auth.workload.jwt_bundle_file")
		add(w.X509BundleFile, "auth.workload.x509_bundle_file")
	}
	if cfg.ControlPlane.Enabled && cfg.ControlPlane.SSHHostKey != "" {
		if k, err := cfg.SSHHostKeyFile(); err == nil {
			add(k, "control_plane.ssh_host_key")
		}
	}
	if cfg.Record.Dir != "" {
		add(cfg.Record.Dir, "record.dir")
	}
	for _, f := range cfg.Record.Also {
		add(f, "record.also")
	}
	return named[p]
}

// checkLinks refuses a state file with a second name: the sandbox guards the
// state by path, so a command could rewrite the file through the other name.
func checkLinks(cfg config.Config, workspace string, stateRoots []string) error {
	roots := append(append([]string{workspace}, cfg.AdditionalDirs...), stateRoots...)
	linked := tools.NewStateSet(roots...).Linked()
	linked = append(linked, StatePaths(cfg, workspace)...)
	// An enclosing repository's config is state too: a run started in a
	// subfolder could rewrite it through a second name inside its workspace.
	for _, r := range roots {
		linked = append(linked, ancestorState(r)...)
	}
	for _, p := range linked {
		n, err := nlink.Linked(p)
		if err != nil {
			return fmt.Errorf("refusing to start: the state file %s cannot be checked: %w", p, err)
		}
		if n > 0 {
			return fmt.Errorf("refusing to start: %w", nlink.Refusal(p, n))
		}
	}
	return nil
}

// ancestorState lists the known state files in the .abhed of every folder above
// dir, as spelled and with links followed, that this user could rewrite. A
// world-writable sticky folder such as /tmp is skipped: another user's file
// there is not ours to fix, and not ours to rewrite either.
func ancestorState(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range []string{abs, tools.RealPath(abs)} {
		for d := filepath.Dir(p); ; d = filepath.Dir(d) {
			if !sharedFolder(d) && !sharedFolder(filepath.Join(d, tools.StateDir)) {
				for _, name := range tools.KnownStateFiles() {
					f := filepath.Join(d, tools.StateDir, name)
					if info, err := os.Lstat(f); err == nil && info.Mode().IsRegular() && !slices.Contains(out, f) && writable(f, info) {
						out = append(out, f)
					}
				}
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return out
}

// sharedFolder reports a world-writable folder with the sticky bit, where
// anyone may leave files.
func sharedFolder(d string) bool {
	info, err := os.Stat(d)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSticky != 0 && info.Mode().Perm()&0o002 != 0
}

// inside reports whether a resolved path lies in one of the resolved dirs.
func inside(real string, dirs []string) bool {
	for _, d := range dirs {
		if rel, err := filepath.Rel(d, real); err == nil && filepath.IsLocal(rel) {
			return true
		}
	}
	return false
}

// under reports whether any spelling of a path lies in any of dirs, each
// dir also compared with its links followed.
func under(spellings, dirs []string) bool {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		for _, dir := range []string{abs, tools.RealPath(abs)} {
			for _, s := range spellings {
				rel, err := filepath.Rel(dir, s)
				if err == nil && filepath.IsLocal(rel) {
					return true
				}
			}
		}
	}
	return false
}

// Isolation is what the bash tool tells the model of the sandbox of tier:
// no network, any host, or, under the allowlist, only the destinations the
// egress rules allow, through the session's proxy.
func Isolation(cfg config.Config, tier string) tools.Isolation {
	iso := tools.Isolation{Tier: tier, Network: cfg.Sandbox.AllowNetwork}
	if cfg.Sandbox.Network != config.NetworkAllowlist {
		return iso
	}
	iso.Network, iso.Allowlist = false, true
	if pol, err := egress.Compile(cfg.Egress); err == nil {
		iso.AllowedHosts, iso.DefaultAllow = pol.AllowedHosts(), pol.DefaultAllow()
	}
	return iso
}
